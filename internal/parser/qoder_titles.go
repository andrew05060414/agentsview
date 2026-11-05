package parser

import (
	"context"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/stringutil"
)

// QoderTitleDBName is the database where the Qoder app stores session titles.
// It lives in the app's Electron user-data directory, outside every session
// root, so a root is bound to it only by an absolute home-anchored path.
const QoderTitleDBName = "main.sqlite"

// qoderTitleLayouts pairs each home session root with the app that owns it.
// The two builds keep separate databases, and one build's session ID is looked
// up only under its own root. appID is the build's data directory name under
// Electron's appData directory, which os.UserConfigDir matches on every OS.
var qoderTitleLayouts = []struct {
	dotDir string
	appID  string
}{
	{qoderDotDir, "com.qoder.app.stable"},
	{qoderCNDotDir, "com.qodercn.app.stable"},
}

const qoderTitleQuery = "SELECT session_id, title FROM chat_sessions"

// QoderStoreTitles reads the titles from one Qoder application database.
func QoderStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "chat_sessions", "session_id", qoderTitleQuery)
}

// QoderSessionRoot returns the canonical default root that owns a transcript
// path, even after the file was deleted. A nested default root takes precedence.
// Membership is lexical under either the unresolved or resolved default root.
func QoderSessionRoot(path string) (string, bool) {
	root, _, ok := qoderSessionTitleStore(path)
	return root, ok
}

func qoderSessionTitleStore(path string) (root, store string, ok bool) {
	owner := ""
	for alias, location := range qoderTitleDatabases() {
		rel, err := filepath.Rel(alias, path)
		if err == nil && rel != "." && filepath.IsLocal(rel) && len(alias) > len(owner) {
			owner, root, store = alias, location.root, location.store
		}
	}
	return root, store, owner != ""
}

// QoderTitleDatabase returns the application database holding titles for a
// configured session root. Only the two home default roots have a known
// title store: a custom root, a Qoder CLI or .qoderwork root, and a remote
// mirror root extracted under a temp directory all return false. Matching a
// trailing path segment would bind the local database to another machine's
// sessions, so the comparison is anchored to this user's home directory.
func QoderTitleDatabase(root string) (string, bool) {
	location, ok := qoderTitleDatabases()[root]
	return location.store, ok
}

type qoderTitleStore struct {
	root  string
	store string
}

func qoderTitleDatabases() map[string]qoderTitleStore {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	appData, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	stores := make(map[string]qoderTitleStore, 2*len(qoderTitleLayouts))
	for _, layout := range qoderTitleLayouts {
		unresolved := filepath.Join(home, layout.dotDir, "projects")
		root, _, err := ResolveProviderRoot(AgentQoder, unresolved)
		if err != nil {
			continue
		}
		location := qoderTitleStore{root: root, store: filepath.Join(appData, layout.appID, QoderTitleDBName)}
		stores[unresolved] = location
		stores[root] = location
	}
	return stores
}

// applyQoderTitles uses a non-blank app database title for the file's own
// session. A successful read with no row preserves the parser's title signal.
// A blank row or read error suppresses that signal, so file-side names seed
// inserts while updates preserve the archived name.
func applyQoderTitles(ctx context.Context, results []ParseResult, path, fileStem string) {
	_, store, ok := qoderSessionTitleStore(path)
	if !ok {
		return
	}
	for i := range results {
		results[i].Session.SessionNamePresent = stringutil.SanitizeUTF8(results[i].Session.SessionName) != ""
	}
	titles, _, err := QoderStoreTitles(ctx, store)
	title, row := "", false
	if err == nil {
		title, row = titles[fileStem]
	}
	if err == nil && !row {
		return
	}
	target := qoderPrefixID(fileStem)
	for i := range results {
		if results[i].Session.ID != target {
			continue
		}
		if row && title != "" {
			results[i].Session.SessionName = title
			results[i].Session.SessionNamePresent = true
			continue
		}
		results[i].Session.SessionNamePresent = false
	}
}
