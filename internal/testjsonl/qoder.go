package testjsonl

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// QoderTitleID is a session ID shaped like the ones Qoder names transcripts
// with: the chat_sessions key is the transcript stem.
const QoderTitleID = "11111111-1111-4111-8111-111111111111"

// QoderProjectDir is the encoded project directory a Qoder transcript lives in.
const QoderProjectDir = "-Users-alice-sample-project"

// QoderTitleRoot isolates the home directory and returns the international
// session root and its application database path.
func QoderTitleRoot(t *testing.T) (root, store string) {
	t.Helper()
	home := IsolateQoderHome(t)
	root = filepath.Join(home, ".qoder", "projects")
	require.NoError(t, os.MkdirAll(root, 0o755))
	store = filepath.Join(QoderAppDataDir(home), "com.qoder.app.stable", "main.sqlite")
	return root, store
}

// IsolateQoderHome points the home and per-user config directories at a fresh
// temporary home and returns it with symlinks resolved.
func IsolateQoderHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	return home
}

// QoderAppDataDir is the Electron appData directory under an isolated home,
// where each Qoder build keeps its data directory.
func QoderAppDataDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming")
	default:
		return filepath.Join(home, ".config")
	}
}

// QoderTitleSchema is the smallest chat_sessions shape the title reader needs.
// The real table has more columns, and the two Qoder builds' column sets are
// not identical, so the reader must select only what it uses.
const QoderTitleSchema = `CREATE TABLE IF NOT EXISTS chat_sessions (
	session_id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	cwd TEXT NOT NULL,
	session_kind TEXT,
	archived INTEGER,
	deleted_at TEXT
)`

// WriteQoderTranscript writes one session transcript under root and returns its
// path. The single user line is enough for the parser to emit one session.
func WriteQoderTranscript(t *testing.T, root, id, content string) string {
	t.Helper()
	path := filepath.Join(root, QoderProjectDir, id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := ClaudeUserWithSessionIDJSON(content, "2026-06-04T09:47:27.966Z", id) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// SetQoderTitle writes one title into the application database at store,
// creating the database and table on first use.
func SetQoderTitle(t *testing.T, store, id string, title any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), QoderTitleSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO chat_sessions (session_id, title, cwd) VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET title = excluded.title`,
		id, title, "/Users/alice/sample-project",
	)
	require.NoError(t, err)
}
