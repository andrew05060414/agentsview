package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// toolRow is an assistant record whose whole content is one tool rendering,
// the shape Codex stores for a function call.
func toolRow(sid string, ordinal int, rendering, inputJSON string) db.Message {
	return db.Message{
		SessionID: sid, Ordinal: ordinal, Role: "assistant",
		Content: rendering, ContentLength: len(rendering), HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "shell", Category: "Bash", InputJSON: inputJSON,
		}},
	}
}

func dialogueText(content string) *string { return &content }

// TestGetMessages_OmitsToolOnlyRows pins the default read to dialogue: a row
// whose content is only a tool rendering is counted in Filtered, while a row
// that still has prose is returned whole.
func TestGetMessages_OmitsToolOnlyRows(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 4
	})
	rendering := "[Bash]\n$ ls"
	prose := "running it\n" + rendering
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		dbtest.UserMsg("s1", 0, "please check"),
		toolRow("s1", 1, rendering, `{"command":"ls"}`),
		{
			SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: prose, ContentLength: len(prose), HasToolUse: true,
			DialogueText: dialogueText("running it"),
			ToolCalls: []db.ToolCall{{
				ToolName: "shell", Category: "Bash", InputJSON: `{"command":"ls"}`,
			}},
		},
		dbtest.AsstMsg("s1", 3, "done"),
	}))

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "s1"})
	require.NoError(t, err)
	require.Len(t, out.Messages, 3)
	assert.Equal(t, []int{0, 2, 3}, ordinals(out))
	assert.Equal(t, prose, out.Messages[1].Content, "prose plus a tool call is returned whole")
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
		dbtest.AsstMsg("s1", 1, "done"),
	}))

	_, out, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "s1", Raw: true})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, ordinals(out))
	assert.Equal(t, 0, out.Filtered)
}

// TestGetMessages_KeepsUnmatchedRendering pins the safe side of the rule: a
// tool row whose rendering cannot be rebuilt from its stored input is kept
// and counted as uncertain, never deleted on a guess.
func TestGetMessages_KeepsUnmatchedRendering(t *testing.T) {
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
	require.Len(t, out.Messages, 1)
	assert.Equal(t, 0, out.Messages[0].Ordinal)
	assert.Equal(t, 1, out.Uncertain)
	assert.Equal(t, 0, out.Filtered)
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
		dbtest.UserMsg("s1", 0, "earlier"),
		toolRow("s1", 1, "[Bash]\n$ a", `{"command":"a"}`),
		toolRow("s1", 2, "[Bash]\n$ b", `{"command":"b"}`),
		dbtest.AsstMsg("s1", 3, "before"),
		toolRow("s1", 4, "[Bash]\n$ c", `{"command":"c"}`),
		dbtest.UserMsg("s1", 5, "anchor"),
		toolRow("s1", 6, "[Bash]\n$ d", `{"command":"d"}`),
		dbtest.AsstMsg("s1", 7, "after"),
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
		dbtest.UserMsg("s1", 0, "earlier"),
		toolRow("s1", 1, "[Bash]\n$ a", `{"command":"a"}`),
		dbtest.UserMsg("s1", 2, "anchor"),
		dbtest.AsstMsg("s1", 3, "after"),
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
