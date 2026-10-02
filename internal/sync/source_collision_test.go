package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const collisionBaseID = "gemini:shared-session"

// geminiCollisionSession renders a Gemini chat file for sessionID with the
// requested number of alternating user/assistant messages.
func geminiCollisionSession(sessionID string, messages int) string {
	msgs := make([]map[string]any, 0, messages)
	for i := range messages {
		id := "m" + string(rune('a'+i))
		if i%2 == 0 {
			msgs = append(msgs, testjsonl.GeminiUserMsg(id, tsEarly, "question "+id))
		} else {
			msgs = append(msgs, testjsonl.GeminiAssistantMsg(id, tsEarlyS5, "answer "+id, nil))
		}
	}
	return testjsonl.GeminiSessionJSON(sessionID, "collisionhash", tsEarly, tsEarlyS5, msgs)
}

// collisionEnv syncs two Gemini files that record the same session id, the
// first in its own pass, and returns the stored path of each. The second file
// sorts first so a rebuild discovers the files in the other order.
func collisionEnv(t *testing.T, firstMessages, secondMessages int) (env *testEnv, first, second string) {
	t.Helper()
	env = setupTestEnv(t)
	first = env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-05-first.json"),
		geminiCollisionSession("shared-session", firstMessages))
	env.engine.SyncAll(t.Context(), nil)
	second = env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-00-second.json"),
		geminiCollisionSession("shared-session", secondMessages))
	env.engine.SyncAll(t.Context(), nil)
	return env, first, second
}

func requireStoredSession(t *testing.T, database *db.DB, id string) *db.Session {
	t.Helper()
	sess, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sess, "session %q", id)
	return sess
}

// assertCollisionPair checks that the base id holds basePath's transcript and
// the other file is a linked continuation under its derived id.
func assertCollisionPair(t *testing.T, database *db.DB, basePath string, baseCount int, altPath string, altCount int) {
	t.Helper()
	base := requireStoredSession(t, database, collisionBaseID)
	require.NotNil(t, base.FilePath)
	assert.Equal(t, basePath, *base.FilePath)
	assertSessionMessageCount(t, database, collisionBaseID, baseCount)

	altID := parser.AltSessionID(collisionBaseID, altPath)
	alt := requireStoredSession(t, database, altID)
	require.NotNil(t, alt.FilePath)
	assert.Equal(t, altPath, *alt.FilePath)
	require.NotNil(t, alt.ParentSessionID)
	assert.Equal(t, collisionBaseID, *alt.ParentSessionID)
	assert.Equal(t, string(parser.RelContinuation), alt.RelationshipType)
	assertSessionMessageCount(t, database, altID, altCount)
}

// Two files that record one session id are both kept. The file stored first
// keeps the id whatever its length; the other becomes a linked session on the
// same pass it appears.
func TestSyncKeepsBothFilesWhenSessionIDsCollide(t *testing.T) {
	tests := []struct {
		name          string
		first, second int
	}{
		{name: "shorter file appears after the longer one", first: 5, second: 1},
		{name: "longer file appears after the shorter one", first: 1, second: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, first, second := collisionEnv(t, tt.first, tt.second)
			assertCollisionPair(t, env.db, first, tt.first, second, tt.second)

			// Later syncs and edits keep each file on its own id.
			env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", filepath.Base(second)),
				geminiCollisionSession("shared-session", tt.second+2))
			env.engine.SyncAll(t.Context(), nil)
			assertCollisionPair(t, env.db, first, tt.first, second, tt.second+2)
		})
	}
}

// A fresh archive that discovers both files in one pass keeps both.
func TestSyncKeepsBothCollidingFilesInOnePass(t *testing.T) {
	env := setupTestEnv(t)
	paths := make([]string, 0, 2)
	for i, name := range []string{"session-2026-01-01T10-00-a.json", "session-2026-01-01T10-05-b.json"} {
		paths = append(paths, env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", name),
			geminiCollisionSession("shared-session", 2*i+1)))
	}
	env.engine.SyncAll(t.Context(), nil)

	base := requireStoredSession(t, env.db, collisionBaseID)
	require.NotNil(t, base.FilePath)
	altPath := paths[0]
	if *base.FilePath == paths[0] {
		altPath = paths[1]
	}
	alt := requireStoredSession(t, env.db, parser.AltSessionID(collisionBaseID, altPath))
	assert.Equal(t, 4, base.MessageCount+alt.MessageCount)
}

// When the owner's file is gone but not yet marked missing, two new files
// in one pass still keep both transcripts.
func TestSyncKeepsBothFilesReplacingMissingOwner(t *testing.T) {
	env := setupTestEnv(t)
	dir := filepath.Join("tmp", "collisionhash", "chats")
	owner := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-owner.json"),
		geminiCollisionSession("shared-session", 5))
	env.engine.SyncAll(t.Context(), nil)
	require.NoError(t, os.Remove(owner))
	paths := []string{
		env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-00-a.json"), geminiCollisionSession("shared-session", 1)),
		env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-05-b.json"), geminiCollisionSession("shared-session", 3)),
	}
	env.engine.SyncAll(t.Context(), nil)

	base := requireStoredSession(t, env.db, collisionBaseID)
	require.NotNil(t, base.FilePath)
	require.Contains(t, paths, *base.FilePath)
	altPath := paths[0]
	if *base.FilePath == paths[0] {
		altPath = paths[1]
	}
	alt := requireStoredSession(t, env.db, parser.AltSessionID(collisionBaseID, altPath))
	assert.Equal(t, 4, base.MessageCount+alt.MessageCount)
}

// A rebuild keeps the previous owner on the base id even when it discovers
// the other file first, so curation on the base stays with its transcript.
func TestResyncKeepsCollisionOwner(t *testing.T) {
	env, first, second := collisionEnv(t, 1, 5)
	starred, err := env.db.StarSession(t.Context(), collisionBaseID)
	require.NoError(t, err)
	require.True(t, starred)
	// A third file the rebuild sees first must not take the base id either.
	third := env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T09-00-third.json"),
		geminiCollisionSession("shared-session", 3))

	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)

	assertCollisionPair(t, env.db, first, 1, second, 5)
	assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, third), 3)
	ids, err := env.db.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, ids, collisionBaseID)
}

// A collision first seen during a rebuild keeps the stored file on the base
// id, with its curation, even when the rebuild discovers the new file first.
func TestResyncMeetingNewFileKeepsStoredOwner(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored", true: "deleted"}[deleted], func(t *testing.T) {
			env := setupTestEnv(t)
			dir := filepath.Join("tmp", "collisionhash", "chats")
			owner := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T10-05-owner.json"),
				geminiCollisionSession("shared-session", 5))
			env.engine.SyncAll(t.Context(), nil)
			if deleted {
				require.NoError(t, env.db.DeleteSession(t.Context(), collisionBaseID))
			} else {
				starred, err := env.db.StarSession(t.Context(), collisionBaseID)
				require.NoError(t, err)
				require.True(t, starred)
			}
			newcomer := env.writeGeminiSession(t, filepath.Join(dir, "session-2026-01-01T09-00-new.json"),
				geminiCollisionSession("shared-session", 1))

			stats := env.engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)

			assertSessionMessageCount(t, env.db, parser.AltSessionID(collisionBaseID, newcomer), 1)
			base, err := env.db.GetSessionFull(t.Context(), collisionBaseID)
			require.NoError(t, err)
			alt, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(collisionBaseID, owner))
			require.NoError(t, err)
			assert.Nil(t, alt, "the stored file never moves to a derived id")
			if deleted {
				assert.Nil(t, base, "the deleted transcript stays deleted")
				return
			}
			require.NotNil(t, base)
			assert.Equal(t, owner, *base.FilePath)
			ids, err := env.db.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, ids, collisionBaseID)
		})
	}
}

// Each file's row follows its own file: deleting one marks only that row
// source-missing, and the survivor keeps its id.
func TestCollidingFileDeletionMarksOnlyItsRow(t *testing.T) {
	t.Run("derived file deleted", func(t *testing.T) {
		env, first, second := collisionEnv(t, 5, 1)
		require.NoError(t, os.Remove(second))
		require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{second}))

		alt := requireStoredSession(t, env.db, parser.AltSessionID(collisionBaseID, second))
		assert.NotNil(t, alt.SourceMissingAt)
		base := requireStoredSession(t, env.db, collisionBaseID)
		assert.Nil(t, base.SourceMissingAt)
		assert.Equal(t, first, *base.FilePath)
	})
	t.Run("base file deleted", func(t *testing.T) {
		env, first, second := collisionEnv(t, 5, 1)
		require.NoError(t, os.Remove(first))
		require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{first}))
		env.writeGeminiSession(t, filepath.Join("tmp", "collisionhash", "chats", filepath.Base(second)),
			geminiCollisionSession("shared-session", 3))
		env.engine.SyncAll(t.Context(), nil)

		base := requireStoredSession(t, env.db, collisionBaseID)
		assert.NotNil(t, base.SourceMissingAt)
		assertCollisionPair(t, env.db, first, 5, second, 3)
	})
}

// Deleting or trashing the base session hides only the base. The other file
// keeps its derived id and stays visible, and the base id is never refilled
// from it, including across a rebuild.
func TestUserDeletedBaseKeepsCollidingFileSeparate(t *testing.T) {
	tests := []struct {
		name   string
		remove func(*db.DB) error
	}{
		{name: "deleted", remove: func(d *db.DB) error { return d.DeleteSession(t.Context(), collisionBaseID) }},
		{name: "trashed", remove: func(d *db.DB) error { return d.SoftDeleteSession(t.Context(), collisionBaseID) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _, second := collisionEnv(t, 5, 1)
			require.NoError(t, tt.remove(env.db))
			altID := parser.AltSessionID(collisionBaseID, second)
			for _, sync := range []func(){
				func() { env.engine.SyncAll(t.Context(), nil) },
				func() { env.engine.ResyncAll(t.Context(), nil) },
			} {
				sync()
				alt := requireStoredSession(t, env.db, altID)
				assert.Nil(t, alt.DeletedAt)
				assertSessionMessageCount(t, env.db, altID, 1)
				base, err := env.db.GetSessionFull(t.Context(), collisionBaseID)
				require.NoError(t, err)
				if base != nil {
					assert.NotNil(t, base.DeletedAt)
					assert.Equal(t, 5, base.MessageCount)
				}
			}
		})
	}
}

// Deleting the derived session excludes only that file; it does not take
// the base id once the base file is gone.
func TestUserDeletedDerivedSessionStaysExcluded(t *testing.T) {
	env, first, second := collisionEnv(t, 5, 1)
	altID := parser.AltSessionID(collisionBaseID, second)
	require.NoError(t, env.db.DeleteSession(t.Context(), altID))
	require.NoError(t, os.Remove(first))
	env.engine.SyncAll(t.Context(), nil)

	alt, err := env.db.GetSessionFull(t.Context(), altID)
	require.NoError(t, err)
	assert.Nil(t, alt)
	base := requireStoredSession(t, env.db, collisionBaseID)
	assert.Equal(t, first, *base.FilePath)
	assertSessionMessageCount(t, env.db, collisionBaseID, 5)
}

// A file that shrinks in place still replaces its own transcript.
func TestSyncAcceptsSameSourceShrinking(t *testing.T) {
	rel := filepath.Join("tmp", "collisionhash", "chats", "session-2026-01-01T10-00-same.json")
	env := setupTestEnv(t)
	env.writeGeminiSession(t, rel, geminiCollisionSession("shrinking-session", 5))
	env.engine.SyncAll(t.Context(), nil)
	assertSessionMessageCount(t, env.db, "gemini:shrinking-session", 5)

	env.writeGeminiSession(t, rel, geminiCollisionSession("shrinking-session", 1))
	env.engine.SyncAll(t.Context(), nil)
	assertSessionMessageCount(t, env.db, "gemini:shrinking-session", 1)
}

// A derived session follows its file when the provider moves it: a second
// project's Cursor .txt transcript replaced by a .jsonl beside it keeps one
// row under the same id, and a deleted one stays deleted.
func TestDerivedSessionFollowsProviderMove(t *testing.T) {
	const baseID = "cursor:shared"
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept", true: "deleted"}[deleted], func(t *testing.T) {
			cursorDir := t.TempDir()
			env := setupTestEnv(t, WithCursorDirs([]string{cursorDir}))
			txt := "user:\nHello\nassistant:\nHi\n"
			env.writeCursorSession(t, cursorDir, "Users-alice-code-one", "shared.txt", txt)
			env.engine.SyncAll(t.Context(), nil)
			second := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.txt", txt)
			env.engine.SyncAll(t.Context(), nil)
			altID := parser.AltSessionID(baseID, second)
			requireStoredSession(t, env.db, altID)
			if deleted {
				require.NoError(t, env.db.DeleteSession(t.Context(), altID))
			}

			jsonl := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.jsonl",
				`{"role":"user","message":{"content":"Hello"}}`+"\n"+
					`{"role":"assistant","message":{"content":"Hi"}}`+"\n"+
					`{"role":"user","message":{"content":"More"}}`+"\n")
			env.engine.SyncAll(t.Context(), nil)

			moved, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(baseID, jsonl))
			require.NoError(t, err)
			assert.Nil(t, moved, "no second derived id for the moved file")
			alt, err := env.db.GetSessionFull(t.Context(), altID)
			require.NoError(t, err)
			if deleted {
				assert.Nil(t, alt, "the deleted session stays deleted")
				return
			}
			require.NotNil(t, alt)
			assert.Equal(t, jsonl, *alt.FilePath)
			assert.Equal(t, 3, alt.MessageCount)
		})
	}
}

// A rebuild resolves a derived session's move the same way an ordinary sync
// does, even after its old file was marked missing.
func TestResyncFollowsMoveOfMissingDerivedSession(t *testing.T) {
	const baseID = "cursor:shared"
	cursorDir := t.TempDir()
	env := setupTestEnv(t, WithCursorDirs([]string{cursorDir}))
	txt := "user:\nHello\nassistant:\nHi\n"
	env.writeCursorSession(t, cursorDir, "Users-alice-code-one", "shared.txt", txt)
	env.engine.SyncAll(t.Context(), nil)
	second := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.txt", txt)
	env.engine.SyncAll(t.Context(), nil)
	altID := parser.AltSessionID(baseID, second)
	starred, err := env.db.StarSession(t.Context(), altID)
	require.NoError(t, err)
	require.True(t, starred)
	require.NoError(t, os.Remove(second))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{second}))
	require.NotNil(t, requireStoredSession(t, env.db, altID).SourceMissingAt)

	jsonl := env.writeCursorSession(t, cursorDir, "Users-alice-code-two", "shared.jsonl",
		`{"role":"user","message":{"content":"Hello"}}`+"\n"+
			`{"role":"assistant","message":{"content":"Hi"}}`+"\n"+
			`{"role":"user","message":{"content":"More"}}`+"\n")
	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)

	moved, err := env.db.GetSessionFull(t.Context(), parser.AltSessionID(baseID, jsonl))
	require.NoError(t, err)
	assert.Nil(t, moved, "no second derived id for the moved file")
	alt := requireStoredSession(t, env.db, altID)
	assert.Equal(t, jsonl, *alt.FilePath)
	assert.Equal(t, 3, alt.MessageCount)
	ids, err := env.db.ListStarredSessionIDs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, ids, altID)
}
