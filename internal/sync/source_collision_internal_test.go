package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// A result moved to a derived id keeps its retry flag, so an incomplete
// parse is not stamped current under the new id.
func TestSourceCollisionKeepsRetryFlag(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "tmp", "hash", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	owner := filepath.Join(chats, "session-2026-01-01T09-00-owner.json")
	other := filepath.Join(chats, "session-2026-01-01T10-00-other.json")
	for _, path := range []string{owner, other} {
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	}
	database := openTestDB(t)
	const id = "gemini:shared"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: id, Project: "p", Machine: "local", Agent: string(parser.AgentGemini), FilePath: &owner,
	}))
	provider, ok := parser.NewProvider(parser.AgentGemini, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	e := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)

	res := processResult{
		results: []parser.ParseResult{{Session: parser.ParsedSession{
			ID: id, Agent: parser.AgentGemini, File: parser.FileInfo{Path: other},
		}}},
		retrySessionIDs: map[string]bool{id: true},
	}
	e.applyProviderFilePathPolicies(t.Context(), provider, parser.AgentGemini, other, &res)

	require.Len(t, res.results, 1)
	altID := parser.AltSessionID(id, other)
	assert.Equal(t, altID, res.results[0].Session.ID)
	assert.True(t, res.needsRetryForSession(altID))
}
