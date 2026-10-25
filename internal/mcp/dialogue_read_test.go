package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

// toolRow is an assistant record whose whole content is one tool rendering,
// the shape Codex stores for a function call.
func toolRow(sid string, ordinal int, rendering, inputJSON string) db.Message {
	return db.Message{
		SessionID: sid, Ordinal: ordinal, Role: "assistant",
		Content: rendering, ContentLength: len(rendering), HasToolUse: true,
		DialogueText: new(""),
		ToolCalls: []db.ToolCall{{
			ToolName: "shell", Category: "Bash", InputJSON: inputJSON,
		}},
	}
}

func dialogueText(content string) *string { return &content }

// These fixtures represent classified dialogue. Legacy coverage-gap fixtures
// deliberately use db.Message or dbtest helpers without these wrappers.
func dialogueUserMsg(sid string, ordinal int, content string) db.Message {
	m := dbtest.UserMsg(sid, ordinal, content)
	m.DialogueText = new(content)
	return m
}

func dialogueAsstMsg(sid string, ordinal int, content string) db.Message {
	m := dbtest.AsstMsg(sid, ordinal, content)
	m.DialogueText = new(content)
	return m
}

func classifiedMessages(messages []db.Message) []db.Message {
	for i := range messages {
		messages[i].DialogueText = new(messages[i].Content)
	}
	return messages
}

func TestProjectedMessageContent_Classification(t *testing.T) {
	for _, tc := range []struct {
		name               string
		message            db.Message
		want               string
		visible, uncertain bool
	}{
		{"mixed", db.Message{Role: "assistant", Content: "answer\n[Thinking] hidden\n[Bash] $ hidden", DialogueText: new("answer"), HasThinking: true, HasToolUse: true}, "answer", true, false},
		{"projection is authoritative", db.Message{Role: "assistant", Content: "[Bash]\n$ cmd", DialogueText: new("classified answer"), HasToolUse: true, ToolCalls: []db.ToolCall{{ToolName: "shell", Category: "Bash", InputJSON: `{"command":"cmd"}`}}}, "classified answer", true, false},
		{"empty", db.Message{Role: "assistant", Content: "[Thinking] hidden", DialogueText: new("")}, "", false, false},
		{"whitespace", db.Message{Role: "assistant", DialogueText: new(" \n")}, "", false, false},
		{"legacy without flags", db.Message{Role: "assistant", Content: "answer\n[Thinking] hidden"}, "", false, true},
		{"legacy user", db.Message{Role: "user", Content: "unclassified"}, "", false, true},
		{"legacy thinking", db.Message{Role: "assistant", Content: "hidden", HasThinking: true}, "", false, true},
		{"source subtype", db.Message{Role: "assistant", Content: "result", DialogueText: new("result"), SourceSubtype: "tool_result"}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, visible, uncertain := projectedMessageContent(tc.message, false)
			assert.Equal(t, tc.want, content)
			assert.Equal(t, tc.visible, visible)
			assert.Equal(t, tc.uncertain, uncertain)
			raw, ok, gap := projectedMessageContent(tc.message, true)
			assert.Equal(t, tc.message.Content, raw)
			assert.True(t, ok)
			assert.False(t, gap)
		})
	}
}

func TestGetMessages_ProjectionContinuation(t *testing.T) {
	for _, around := range []bool{false, true} {
		for _, raw := range []bool{false, true} {
			t.Run(fmt.Sprintf("around=%t/raw=%t", around, raw), func(t *testing.T) {
				ts, d := newTestToolset(t)
				body := "αβγδεζηθικ"
				stored := "[Thinking] hidden\n" + body + "\n[Bash]\n$ hidden"
				dbtest.SeedSessionWithMessages(t, d, "projection", "proj", []db.Message{
					{SessionID: "projection", Ordinal: 0, Role: "assistant", Content: stored, DialogueText: new(body), HasThinking: true, HasToolUse: true},
					dialogueUserMsg("projection", 1, "next"),
				}, dbtest.WithMessageCounts(2, 1))
				in := getMessagesIn{SessionID: "projection", Limit: 1, Raw: raw, MaxCharsPerMessage: 4}
				if around {
					in.Around, in.Before, in.After = new(0), new(0), new(0)
				}
				_, page, err := ts.getMessages(t.Context(), nil, in)
				require.NoError(t, err)
				require.Len(t, page.Messages, 1)
				first := page
				parts := []string{page.Messages[0].Content}
				for page.Messages[0].BodyCursor != "" {
					cursor, err := decodeMessageBodyCursor(page.Messages[0].BodyCursor)
					require.NoError(t, err)
					assert.Equal(t, raw, cursor.Raw)
					assert.NotEmpty(t, cursor.EvidenceSource)
					// A request flag cannot switch a dialogue cursor into raw mode.
					_, page, err = ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: in.SessionID, BodyCursor: page.Messages[0].BodyCursor, MaxCharsPerMessage: 4, Raw: !raw})
					require.NoError(t, err)
					require.Len(t, page.Messages, 1)
					assert.Equal(t, first.TranscriptRevision, page.TranscriptRevision)
					parts = append(parts, page.Messages[0].Content)
				}
				want := body
				if raw {
					want = stored
				}
				assert.Equal(t, want, strings.Join(parts, ""))
				assert.Equal(t, len([]rune(want)), first.Messages[0].FullLength)
				require.NotNil(t, page.NextFrom)
				assert.Equal(t, 1, *page.NextFrom)
				_, next, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: in.SessionID, From: page.NextFrom})
				require.NoError(t, err)
				require.Len(t, next.Messages, 1)
				assert.Equal(t, "next", next.Messages[0].Content)
				// A dialogue-only change does not bump the archive transcript
				// revision. Its view digest must still invalidate dialogue cursors;
				// raw cursors continue because the stored raw body is unchanged.
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "projection", []db.Message{
					{SessionID: "projection", Ordinal: 0, Role: "assistant", Content: stored, DialogueText: new("changed dialogue"), HasThinking: true, HasToolUse: true},
					dialogueUserMsg("projection", 1, "next"),
				}))
				_, _, err = ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: in.SessionID, BodyCursor: first.Messages[0].BodyCursor})
				if raw {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, service.ErrSourceChanged)
				}
			})
		}
	}
}

func TestDialogueProjectionWirePreservesClassification(t *testing.T) {
	for _, projection := range []*string{nil, new(""), new("answer")} {
		encoded, err := json.Marshal(db.Message{DialogueText: projection})
		require.NoError(t, err)
		var decoded db.Message
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		assert.Equal(t, projection, decoded.DialogueText)
	}
}

func TestGetMessages_RejectsOldProjectionCursor(t *testing.T) {
	_, err := decodeMessageBodyCursor(encodeMessageBodyCursor(messageBodyCursor{
		Version: 2, EvidenceSource: "archive", SessionID: "session", Revision: "rev", Offset: 1,
	}))
	require.ErrorContains(t, err, "invalid body_cursor")
}

func TestGetMessages_CoverageGapsAndAround(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSessionWithMessages(t, d, "gaps", "proj", []db.Message{
		dialogueUserMsg("gaps", 0, "before"),
		{SessionID: "gaps", Ordinal: 1, Role: "assistant", Content: "[Thinking] unclassified"},
		dialogueAsstMsg("gaps", 2, "after"),
		{SessionID: "gaps", Ordinal: 3, Role: "assistant", Content: "tool result", SourceSubtype: "tool_result", DialogueText: new("tool result")},
	}, dbtest.WithMessageCounts(4, 1))
	_, first, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "gaps", Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []int{0}, ordinals(first))
	assert.Equal(t, 1, first.Filtered)
	assert.Equal(t, 1, first.Uncertain)
	require.NotNil(t, first.NextFrom)
	assert.Equal(t, 2, *first.NextFrom)
	_, second, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "gaps", From: first.NextFrom, Limit: 3, ExpectedRevision: first.TranscriptRevision})
	require.NoError(t, err)
	assert.Equal(t, []int{2}, ordinals(second))
	assert.Equal(t, 4, len(first.Messages)+first.Filtered+len(second.Messages)+second.Filtered)
	assert.Nil(t, second.NextFrom)
	_, around, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "gaps", Around: new(1), Before: new(1), After: new(1)})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 2}, ordinals(around))
	assert.Equal(t, 1, around.Filtered)
	assert.Equal(t, 1, around.Uncertain)
	assert.Equal(t, first.TranscriptRevision, around.TranscriptRevision)
}

func TestDialogueProjection_HTTPReadAndSearchContext(t *testing.T) {
	direct, d := newTestToolset(t)
	dbtest.SeedSessionWithMessages(t, d, "context", "proj", []db.Message{
		{SessionID: "context", Ordinal: 0, Role: "assistant", Content: "before\n[Thinking] hidden\n[Bash] $ hidden", DialogueText: new("before"), HasThinking: true, HasToolUse: true},
		dialogueUserMsg("context", 1, "needle"),
		{SessionID: "context", Ordinal: 2, Role: "assistant", Content: "[Thinking] legacy"},
		dialogueAsstMsg("context", 3, "after"),
	}, dbtest.WithMessageCounts(4, 1))
	// Use the actual service JSON types, as the remote HTTP backend does.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			filter := service.MessageFilter{}
			if r.URL.Query().Get("around") != "" {
				filter.Around, filter.Before, filter.After = new(1), new(0), new(0)
			}
			out, err := direct.svc.Messages(r.Context(), "context", filter)
			require.NoError(t, err)
			require.NoError(t, json.MarshalWrite(w, out))
			return
		}
		out, err := direct.svc.SearchContent(r.Context(), service.ContentSearchRequest{Pattern: "needle", Mode: "substring", Sources: []string{"messages"}, Context: 3, IncludeOneShot: true, IncludeAutomated: true})
		require.NoError(t, err)
		require.NoError(t, json.MarshalWrite(w, out))
	}))
	t.Cleanup(server.Close)
	remote := &toolset{svc: servicehttp.NewHTTPBackend(server.URL, "", false, "")}
	for _, ts := range []*toolset{direct, remote} {
		_, page, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "context"})
		require.NoError(t, err)
		assert.Equal(t, []int{0, 1, 3}, ordinals(page))
		assert.Equal(t, "before", page.Messages[0].Content)
		assert.Equal(t, 1, page.Uncertain)
		_, found, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "needle", Context: 3, IncludeActive: true, IncludeOneShot: true, IncludeAutomated: true})
		require.NoError(t, err)
		require.Len(t, found.Matches, 1)
		assert.Equal(t, []contextMessage{{Ordinal: 0, Role: "assistant", Content: "before"}}, found.Matches[0].ContextBefore)
		assert.Equal(t, []contextMessage{{Ordinal: 3, Role: "assistant", Content: "after"}}, found.Matches[0].ContextAfter)
	}
}

func TestSearchContent_WithholdsLegacySnippets(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSessionWithMessages(t, d, "legacy-search", "proj", []db.Message{
		dialogueUserMsg("legacy-search", 0, "question"),
		{SessionID: "legacy-search", Ordinal: 1, Role: "assistant", Content: "needle\n[Thinking] hidden\n[Bash] $ hidden"},
		{SessionID: "legacy-search", Ordinal: 2, Role: "assistant", Content: "needle\n[Bash] $ hidden", DialogueText: new("needle")},
	}, dbtest.WithMessageCounts(3, 1))
	_, found, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "needle", IncludeActive: true, IncludeOneShot: true})
	require.NoError(t, err)
	require.Len(t, found.Matches, 2)
	for _, match := range found.Matches {
		if match.Ordinal == 1 {
			assert.Empty(t, match.Snippet)
			assert.True(t, match.DialogueUnavailable)
		} else {
			assert.Equal(t, "needle", match.Snippet)
			assert.False(t, match.DialogueUnavailable)
		}
	}
	// Explicit source inspection retains storage's original snippet contract.
	_, explicit, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "needle", Sources: []string{"messages"}, IncludeActive: true, IncludeOneShot: true})
	require.NoError(t, err)
	require.Len(t, explicit.Matches, 2)
	assert.True(t, strings.Contains(explicit.Matches[0].Snippet, "[Thinking]") || strings.Contains(explicit.Matches[1].Snippet, "[Thinking]"))
}

// TestGetMessages_OmitsToolOnlyRows pins the default read to dialogue: a row
// whose content is only a tool rendering is counted in Filtered, while a row
// with prose and tools returns only the dialogue projection.
func TestGetMessages_OmitsToolOnlyRows(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 4
	})
	rendering := "[Bash]\n$ ls"
	prose := "running it\n" + rendering
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dialogueUserMsg("s1", 0, "please check"),
		toolRow("s1", 1, rendering, `{"command":"ls"}`),
		{
			SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: prose, ContentLength: len(prose), HasToolUse: true,
			DialogueText: dialogueText("running it"),
			ToolCalls: []db.ToolCall{{
				ToolName: "shell", Category: "Bash", InputJSON: `{"command":"ls"}`,
			}},
		},
		dialogueAsstMsg("s1", 3, "done"),
	}))

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "s1"})
	require.NoError(t, err)
	require.Len(t, out.Messages, 3)
	assert.Equal(t, []int{0, 2, 3}, ordinals(out))
	assert.Equal(t, "running it", out.Messages[1].Content, "mixed tool rendering must not leak")
	assert.Equal(t, 1, out.Filtered)
	assert.Equal(t, 0, out.Uncertain)
}

// TestGetMessages_RawKeepsToolOnlyRows pins the escape hatch: raw returns the
// stored records, tool-only rows included.
func TestGetMessages_RawKeepsToolOnlyRows(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 2
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		toolRow("s1", 0, "[Bash]\n$ ls", `{"command":"ls"}`),
		dialogueAsstMsg("s1", 1, "done"),
	}))

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "s1", Raw: true})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, ordinals(out))
	assert.Equal(t, 0, out.Filtered)
}

// TestGetMessages_ExcludesUnmatchedRendering ensures uncertain legacy tool text
// is withheld rather than being advertised as dialogue.
func TestGetMessages_ExcludesUnmatchedRendering(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 1
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		Content: "[Bash]\n$ ls", ContentLength: 12, HasToolUse: true,
		ToolCalls: []db.ToolCall{{ToolName: "shell", Category: "Bash"}},
	}}))

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "s1"})
	require.NoError(t, err)
	require.Empty(t, out.Messages)
	assert.Equal(t, 1, out.Uncertain)
	assert.Equal(t, 1, out.Filtered)
}

// A mixed legacy row without a persisted dialogue projection cannot be
// safely decontaminated. Default is fail-closed, raw remains available.
func TestGetMessages_WithholdsUnclassifiedMixedRow(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "mixed", "proj", func(s *db.Session) {
		s.MessageCount = 2
	})
	raw := "starting\\n[Bash]\\n$ cat token"
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "mixed", Ordinal: 0, Role: "assistant", Content: raw,
			ContentLength: len(raw), HasToolUse: true,
			ToolCalls: []db.ToolCall{{ToolName: "shell", Category: "Bash", InputJSON: `{"command":"cat token"}`}}},
		dialogueAsstMsg("mixed", 1, "finished"),
	}))
	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "mixed"})
	require.NoError(t, err)
	assert.Equal(t, []int{1}, ordinals(out))
	assert.Equal(t, 1, out.Filtered)
	assert.Equal(t, 1, out.Uncertain)
	_, full, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "mixed", Raw: true})
	require.NoError(t, err)
	require.Len(t, full.Messages, 2)
	assert.Equal(t, raw, full.Messages[0].Content)
}

// TestGetMessages_AroundCountsVisibleRows pins the window to dialogue rows:
// tool-only rows between the anchor and its neighbours do not consume the
// requested before/after counts.
func TestGetMessages_AroundCountsVisibleRows(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 9
	})
	msgs := []db.Message{
		dialogueUserMsg("s1", 0, "earlier"),
		toolRow("s1", 1, "[Bash]\n$ a", `{"command":"a"}`),
		toolRow("s1", 2, "[Bash]\n$ b", `{"command":"b"}`),
		dialogueAsstMsg("s1", 3, "before"),
		toolRow("s1", 4, "[Bash]\n$ c", `{"command":"c"}`),
		dialogueUserMsg("s1", 5, "anchor"),
		toolRow("s1", 6, "[Bash]\n$ d", `{"command":"d"}`),
		dialogueAsstMsg("s1", 7, "after"),
		toolRow("s1", 8, "[Bash]\n$ e", `{"command":"e"}`),
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))

	anchor, before, after := 5, 1, 1
	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "s1", Around: &anchor, Before: &before, After: &after,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{3, 5, 7}, ordinals(out))
	assert.Equal(t, 2, out.Filtered, "the tool rows between the visible ones are counted")
	require.NotNil(t, out.NextFrom)
	assert.Equal(t, 8, *out.NextFrom, "next_from anchors on the last scanned raw ordinal")
}

// TestGetMessages_AroundRawCountsStoredRows pins raw to the old window: it
// counts stored rows, so the tool-only neighbour fills the requested context.
func TestGetMessages_AroundRawCountsStoredRows(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 4
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dialogueUserMsg("s1", 0, "earlier"),
		toolRow("s1", 1, "[Bash]\n$ a", `{"command":"a"}`),
		dialogueUserMsg("s1", 2, "anchor"),
		dialogueAsstMsg("s1", 3, "after"),
	}))

	anchor, before, after := 2, 1, 1
	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{
		SessionID: "s1", Around: &anchor, Before: &before, After: &after, Raw: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, ordinals(out))
	assert.Equal(t, 0, out.Filtered)
}

// TestLexicalSourcesDefault pins the search default: substring and regex
// narrow to dialogue messages unless the caller names sources, while the
// modes that already search messages only are left untouched.
func TestLexicalSourcesDefault(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"messages"}, lexicalSources("", nil))
	assert.Equal(t, []string{"messages"}, lexicalSources("substring", nil))
	assert.Equal(t, []string{"messages"}, lexicalSources("regex", nil))
	assert.Nil(t, lexicalSources("semantic", nil))
	assert.Nil(t, lexicalSources("hybrid", nil))
	assert.Nil(t, lexicalSources("terms", nil))
	named := []string{"messages", "tool_input"}
	assert.Equal(t, named, lexicalSources("substring", named))
}

func ordinals(out getMessagesOut) []int {
	got := make([]int, len(out.Messages))
	for i, m := range out.Messages {
		got[i] = m.Ordinal
	}
	return got
}
