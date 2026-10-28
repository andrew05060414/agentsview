package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/mask"
)

// Fake secrets are assembled at run time so no complete key sits in the source.
func fakeAnthropicKey() string {
	return "sk-" + "ant-" + "api03-" + "Zx9Qw8Er7" + "Ty6Ui5Op4" + "As3Df2Gh1Jk0"
}
func fakeGoogleKey() string { return "AI" + "za" + "SyD3mo" + "-abcdefghijklmnopqrstuvwxyz01" }
func fakeBearer() string    { return "tok_" + "Zq81Lm40Xv73Bn55Pw" }

func useTestMasker(t *testing.T, o mask.Options) {
	t.Helper()
	m, err := mask.NewDefault(o)
	require.NoError(t, err)
	t.Cleanup(mask.SetDefaultForTest(m))
}

func TestStorageProjectionMasksSecrets(t *testing.T) {
	useTestMasker(t, mask.Options{})
	d := testDB(t)
	ctx := t.Context()

	title := "debug " + fakeGoogleKey()
	insertSession(t, d, "m1", "p", func(s *Session) {
		s.FirstMessage = &title
		s.MessageCount = 2
	})

	user := userMsg("m1", 0, "my key is "+fakeAnthropicKey()+" please check")
	user.ThinkingText = "thinking about " + fakeAnthropicKey()
	user.HasThinking = true
	asst := asstMsg("m1", 1, "ran it")
	asst.HasToolUse = true
	asst.ToolCalls = []ToolCall{{
		ToolName:      "Bash",
		Category:      "Bash",
		ToolUseID:     "tu1",
		InputJSON:     `{"command":"curl -H 'Authorization: Bearer ` + fakeBearer() + `' https://x"}`,
		ResultContent: "ok " + fakeGoogleKey(),
		ResultEvents: []ToolResultEvent{{
			Source: "tool_result", Status: "completed",
			Content: "event " + fakeGoogleKey(),
		}},
	}}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "m1", []Message{user, asst}))

	got, err := d.GetAllMessages(ctx, "m1")
	require.NoError(t, err)
	require.Len(t, got, 2)

	phAnt := mask.Placeholder(fakeAnthropicKey())
	phGoogle := mask.Placeholder(fakeGoogleKey())
	assert.Equal(t, "my key is "+phAnt+" please check", got[0].Content)
	assert.Equal(t, len(got[0].Content), got[0].ContentLength, "content_length follows the masked text")
	assert.Equal(t, "thinking about "+phAnt, got[0].ThinkingText)
	require.Len(t, got[1].ToolCalls, 1)
	tc := got[1].ToolCalls[0]
	assert.NotContains(t, tc.InputJSON, fakeBearer())
	assert.Contains(t, tc.InputJSON, mask.Placeholder(fakeBearer()))
	assert.True(t, strings.HasPrefix(tc.ResultContent, "ok "+phGoogle), "result: %q", tc.ResultContent)
	assert.Equal(t, len(tc.ResultContent), tc.ResultContentLength)
	for _, ev := range tc.ResultEvents {
		assert.NotContains(t, ev.Content, fakeGoogleKey())
		assert.Equal(t, len(ev.Content), ev.ContentLength)
	}

	sess, err := d.GetSession(ctx, "m1")
	require.NoError(t, err)
	require.NotNil(t, sess.FirstMessage)
	assert.Equal(t, "debug "+phGoogle, *sess.FirstMessage)

	// The caller's message values are not modified in place.
	assert.Contains(t, user.Content, fakeAnthropicKey())
	assert.Contains(t, asst.ToolCalls[0].InputJSON, fakeBearer())
}

func TestFullTextIndexHoldsNoPlaintext(t *testing.T) {
	useTestMasker(t, mask.Options{})
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "m2", "p")
	require.NoError(t, d.ReplaceSessionMessages(ctx, "m2", []Message{
		userMsg("m2", 0, "uniqueword "+fakeAnthropicKey()),
	}))
	var n int
	require.NoError(t, d.Reader().QueryRow(ctx,
		`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'uniqueword'`).Scan(&n))
	assert.Equal(t, 1, n, "non-secret words stay searchable")
	require.NoError(t, d.Reader().QueryRow(ctx,
		`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, `"Zx9Qw8Er7Ty6Ui5Op4As3Df2Gh1Jk0"`).Scan(&n))
	assert.Equal(t, 0, n, "the secret's tokens are not indexed")
}

// A second sync of an unchanged source must not rewrite rows: the stored
// (masked) rows must compare equal to the freshly projected input.
func TestReplaceSessionMessagesIsStableWithMasking(t *testing.T) {
	useTestMasker(t, mask.Options{})
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "m3", "p")
	build := func() []Message {
		a := asstMsg("m3", 1, "done "+fakeGoogleKey())
		a.ToolCalls = []ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "t",
			InputJSON: `{"k":"` + fakeBearer() + `"}`, ResultContent: "r " + fakeGoogleKey()}}
		return []Message{userMsg("m3", 0, "key "+fakeAnthropicKey()), a}
	}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "m3", build()))
	first, err := d.GetAllMessages(ctx, "m3")
	require.NoError(t, err)

	require.NoError(t, d.ReplaceSessionMessages(ctx, "m3", build()))
	second, err := d.GetAllMessages(ctx, "m3")
	require.NoError(t, err)
	require.Len(t, second, len(first))
	for i := range first {
		assert.Equal(t, first[i].ID, second[i].ID, "row %d was rewritten", i)
		assert.Equal(t, first[i].Content, second[i].Content)
	}
}

func TestMaskingCanBeSwitchedOff(t *testing.T) {
	t.Cleanup(mask.SetDefaultForTest(nil))
	d := testDB(t)
	insertSession(t, d, "m4", "p")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "m4", []Message{
		userMsg("m4", 0, "key "+fakeAnthropicKey()),
	}))
	got, err := d.GetAllMessages(t.Context(), "m4")
	require.NoError(t, err)
	assert.Contains(t, got[0].Content, fakeAnthropicKey())
}

// disableIngestMasking switches ingestion-time masking off for a test that must
// store a plaintext secret, for example to exercise scanning of legacy rows.
func disableIngestMasking(t *testing.T) {
	t.Helper()
	t.Cleanup(mask.SetDefaultForTest(nil))
}
