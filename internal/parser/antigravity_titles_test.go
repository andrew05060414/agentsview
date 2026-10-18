package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAntigravityTitleCacheExpiresMatchingMarkers(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	priorNow := sqliteTitleNow
	sqliteTitleNow = func() time.Time { return now }
	t.Cleanup(func() { sqliteTitleNow = priorNow })
	store := filepath.Join(t.TempDir(), AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; INSERT INTO conversation_summaries VALUES ('conversation-a', 'First')`)
	require.NoError(t, err)
	checkpoint := func() {
		_, err := conn.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
		require.NoError(t, err)
		for _, path := range []string{store, store + "-wal"} {
			require.NoError(t, os.Chtimes(path, now, now))
		}
	}
	checkpoint()
	titles, _, err := AntigravityStoreTitles(t.Context(), store)
	require.NoError(t, err)
	require.Equal(t, "First", titles["conversation-a"])
	before, ok := StatSQLiteContainerState(store)
	require.True(t, ok)
	_, err = conn.ExecContext(t.Context(), "UPDATE conversation_summaries SET title = 'Later'")
	require.NoError(t, err)
	checkpoint()
	after, ok := StatSQLiteContainerState(store)
	require.True(t, ok)
	require.Equal(t, before, after)
	titles, _, err = AntigravityStoreTitles(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "First", titles["conversation-a"])
	now = now.Add(SQLiteContainerDigestVerifyInterval)
	titles, _, err = AntigravityStoreTitles(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "Later", titles["conversation-a"])
}

func TestParseAntigravityWorkspaceURI(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "unix file uri", raw: "file:///home/user/code/myproject", want: filepath.Clean("/home/user/code/myproject")},
		{name: "windows file uri with encoded colon", raw: "file:///d%3A/synthetic-workspace", want: filepath.Clean(filepath.FromSlash("d:/synthetic-workspace"))},
		{name: "windows file uri with plain colon", raw: "file:///d:/synthetic-corp/projects/app", want: filepath.Clean(filepath.FromSlash("d:/synthetic-corp/projects/app"))},
		{name: "windows unc network share", raw: "file://server/share/repo", want: filepath.Clean(filepath.FromSlash("//server/share/repo"))},
		{name: "windows unc network share localhost", raw: "file://localhost/d:/local/repo", want: filepath.Clean(filepath.FromSlash("d:/local/repo"))},
		{name: "encoded unicode path", raw: "file:///c%3A/%E6%96%B0%E5%BB%BA%E6%96%87%E4%BB%B6%E5%A4%B9", want: filepath.Clean(filepath.FromSlash("c:/新建文件夹"))},
		{name: "plain path", raw: "/var/log/app", want: filepath.Clean("/var/log/app")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAntigravityWorkspaceURI(tc.raw)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAntigravityStoreSummariesWithWorkspace(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	defer conn.Close()

	schema := `CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		workspace_uris TEXT NOT NULL DEFAULT '',
		parent_conversation_id TEXT NOT NULL DEFAULT ''
	)`
	_, err = conn.ExecContext(t.Context(), schema)
	require.NoError(t, err)

	targetID := "synthetic-conv-uuid-1"
	parentID := "synthetic-parent-uuid-1"
	workspaceJSON := `["file:///d%3A/synthetic-workspace"]`
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO conversation_summaries (conversation_id, title, workspace_uris, parent_conversation_id)
		 VALUES (?, ?, ?, ?)`,
		targetID, "Synthetic Task Title", workspaceJSON, parentID,
	)
	require.NoError(t, err)

	summaries, _, err := AntigravityStoreSummaries(t.Context(), store)
	require.NoError(t, err)
	require.Contains(t, summaries, targetID)

	s := summaries[targetID]
	assert.Equal(t, "Synthetic Task Title", s.Title)
	assert.Equal(t, filepath.Clean(filepath.FromSlash("d:/synthetic-workspace")), s.Cwd)
	assert.Equal(t, "synthetic_workspace", s.Project)
	assert.Equal(t, parentID, s.ParentConversationID)

	// Verify applyAntigravitySummary populates ParsedSession
	sess := &ParsedSession{ID: "antigravity:" + targetID}
	applyAntigravitySummary(t.Context(), sess, antigravitySource{Root: root, ID: targetID})
	assert.Equal(t, "Synthetic Task Title", sess.SessionName)
	assert.True(t, sess.SessionNamePresent)
	assert.Equal(t, filepath.Clean(filepath.FromSlash("d:/synthetic-workspace")), sess.Cwd)
	assert.Equal(t, "synthetic_workspace", sess.Project)
	assert.Equal(t, "antigravity:"+parentID, sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
}
