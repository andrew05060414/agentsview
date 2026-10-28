package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/mask"
)

func TestMaskBackfillMasksStoredRows(t *testing.T) {
	disableIngestMasking(t) // store plaintext on purpose
	d := testDB(t)
	ctx := t.Context()

	secret := fakeAnthropicKey()
	bare := "Qm" + "7Hd2Lp9Xv4Bn6Zt1Kc8Rj3Wf5Ys0Ug" // a value only known through the exact list
	title := "debug " + fakeGoogleKey()
	insertSession(t, d, "bf1", "p", func(s *Session) { s.FirstMessage = &title; s.MessageCount = 2 })

	user := userMsg("bf1", 0, "my key is "+secret+" and "+bare+" again")
	asst := asstMsg("bf1", 1, "ok")
	asst.HasToolUse = true
	asst.ToolCalls = []ToolCall{{
		ToolName: "Bash", Category: "Bash", ToolUseID: "tu",
		InputJSON:     `{"command":"echo ` + bare + `"}`,
		ResultContent: "got " + secret,
		ResultEvents:  []ToolResultEvent{{Source: "tool_result", Status: "completed", Content: "event " + fakeGoogleKey()}},
	}, {
		// the summary equals the only event: stored with an empty result_content
		ToolName: "Bash", Category: "Bash", ToolUseID: "tu2",
		ResultContent: "same " + secret,
		ResultEvents:  []ToolResultEvent{{Source: "tool_result", Status: "completed", Content: "same " + secret}},
	}}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "bf1", []Message{user, asst}))
	require.NoError(t, d.ReplaceSessionSecretFindings(ctx, "bf1", []SecretFinding{{
		SessionID: "bf1", RuleName: "anthropic-key", Confidence: "definite", LocationKind: "message",
		MessageOrdinal: 0, MatchStart: len("my key is "), MatchEnd: len("my key is ") + len(secret),
		RedactedMatch: "sk-ant-...Jk0", RulesVersion: "t",
	}}, 1, "t"))
	require.NoError(t, d.SetSyncState(ctx, "last_push_boundary_state:default", `{"fingerprints":{"bf1":"title `+secret+`"}}`))

	// The conversation export projection keeps sha256/length of each message text.
	sum := sha256.Sum256([]byte(user.Content))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages
			(session_id, message_id, ordinal, role, source_id, digest, text_bytes) VALUES (?, 1, 0, 'user', 's', ?, ?)`,
			"bf1", hex.EncodeToString(sum[:]), len(user.Content))
		return err
	}))

	rules, err := mask.LoadRules()
	require.NoError(t, err)
	rep, err := d.MaskBackfill(ctx, MaskBackfillOptions{
		Rules: rules, Apply: true, Workers: 2,
		Exact: []mask.ExactValue{{Label: "unit", Value: bare, Strict: true}},
	})
	require.NoError(t, err)
	require.NoError(t, d.MaskRebuildIndexes(ctx))
	assert.Equal(t, 1, rep.Findings.Rows)

	phSecret, phBare, phGoogle := mask.Placeholder(secret), mask.Placeholder(bare), mask.Placeholder(fakeGoogleKey())
	got, err := d.GetAllMessages(ctx, "bf1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "my key is "+phSecret+" and "+phBare+" again", got[0].Content)
	assert.Equal(t, len(got[0].Content), got[0].ContentLength)
	tc := got[1].ToolCalls[0]
	assert.Equal(t, `{"command":"echo `+phBare+`"}`, tc.InputJSON)
	assert.Equal(t, "got "+phSecret, tc.ResultContent)
	assert.Equal(t, len(tc.ResultContent), tc.ResultContentLength)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "event "+phGoogle, tc.ResultEvents[0].Content)
	assert.Equal(t, len(tc.ResultEvents[0].Content), tc.ResultEvents[0].ContentLength)
	// Deduplicated summary: the row keeps an empty result_content and the length
	// follows the masked event text.
	var rawContent sql.NullString
	var rawLen int
	require.NoError(t, d.Reader().QueryRow(ctx,
		`SELECT result_content, result_content_length FROM tool_calls WHERE session_id = 'bf1' AND tool_use_id = 'tu2'`).Scan(&rawContent, &rawLen))
	assert.Empty(t, rawContent.String)
	assert.Equal(t, len("same "+phSecret), rawLen,
		"the summary length of a deduplicated call follows the masked event text")

	sess, err := d.GetSession(ctx, "bf1")
	require.NoError(t, err)
	assert.Equal(t, "debug "+phGoogle, *sess.FirstMessage)

	// Findings keep no prefix or suffix of the key.
	fs, err := d.SessionSecretFindings(ctx, "bf1")
	require.NoError(t, err)
	require.Len(t, fs, 1)
	assert.Equal(t, phSecret, fs[0].RedactedMatch)

	// Derived bookkeeping that copies text is masked too.
	state, err := d.GetSyncState(ctx, "last_push_boundary_state:default")
	require.NoError(t, err)
	assert.Equal(t, `{"fingerprints":{"bf1":"title `+phSecret+`"}}`, state)

	// Conversation projection digest follows the masked text.
	var digest string
	var textBytes int
	require.NoError(t, d.Reader().QueryRow(ctx,
		`SELECT digest, text_bytes FROM conversation_messages WHERE session_id='bf1' AND ordinal=0`).Scan(&digest, &textBytes))
	want := sha256.Sum256([]byte(got[0].Content))
	assert.Equal(t, hex.EncodeToString(want[:]), digest)
	assert.Equal(t, len(got[0].Content), textBytes)

	// The full-text index no longer holds the secret's tokens.
	var n int
	require.NoError(t, d.Reader().QueryRow(ctx,
		`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '"Zx9Qw8Er7Ty6Ui5Op4As3Df2Gh1Jk0"'`).Scan(&n))
	assert.Zero(t, n)
	checks, err := d.MaskVerify(ctx, false)
	require.NoError(t, err)
	for k, v := range checks {
		assert.NotContains(t, v, "FAIL", k)
	}

	// Idempotent: a second run finds nothing to change.
	rep2, err := d.MaskBackfill(ctx, MaskBackfillOptions{
		Rules: rules, Apply: true, Workers: 2,
		Exact: []mask.ExactValue{{Label: "unit", Value: bare, Strict: true}},
	})
	require.NoError(t, err)
	assert.Zero(t, rep2.Final.Spans)
	assert.Empty(t, rep2.Updates)
}
