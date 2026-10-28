package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/mask"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A synced session never stores the secret it contains, still records that it
// leaked one (findings carry the placeholder, not a fragment of the key), and a
// second sync of the unchanged source rewrites nothing.
func TestSyncMasksSecretsAtIngestion(t *testing.T) {
	m, err := mask.NewDefault(mask.Options{})
	require.NoError(t, err)
	t.Cleanup(mask.SetDefaultForTest(m))

	awsKey := "AKIA" + "7QHWN2DKR4FYPLJM"
	googleKey := "AI" + "za" + "SyD3mo" + "-abcdefghijklmnopqrstuvwxyz01"

	root := t.TempDir()
	projectDir := filepath.Join(root, "proj-a")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "session.jsonl")
	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("please use key "+awsKey+" for the deploy", "2024-01-01T10:00:00Z"),
		testjsonl.ClaudeAssistantJSON("ok, ran it with "+googleKey, "2024-01-01T10:00:01Z"),
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)

	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	ids, err := database.ListSessionIDsByFilePath(t.Context(), path, "claude")
	require.NoError(t, err)
	require.Len(t, ids, 1)

	msgs, err := database.GetAllMessages(t.Context(), ids[0])
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "please use key "+mask.Placeholder(awsKey)+" for the deploy", msgs[0].Content)
	assert.Equal(t, len(msgs[0].Content), msgs[0].ContentLength)
	assert.NotContains(t, msgs[1].Content, googleKey)
	assert.Contains(t, msgs[1].Content, mask.Placeholder(googleKey))

	sess, err := database.GetSession(t.Context(), ids[0])
	require.NoError(t, err)
	require.NotNil(t, sess.FirstMessage)
	assert.NotContains(t, *sess.FirstMessage, awsKey)

	findings, err := database.SessionSecretFindings(t.Context(), ids[0])
	require.NoError(t, err)
	require.NotEmpty(t, findings, "a masked session still records that it leaked a secret")
	for _, f := range findings {
		assert.Contains(t,
			[]string{mask.Placeholder(awsKey), mask.Placeholder(googleKey)},
			f.RedactedMatch, "finding %s keeps no fragment of the key", f.RuleName)
	}

	again := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, again.Synced, "an unchanged source must not be rewritten")
}
