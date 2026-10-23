package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestQoderTitleDatabase(t *testing.T) {
	home := testjsonl.IsolateQoderHome(t)
	appData := testjsonl.QoderAppDataDir(home)
	for _, tc := range []struct {
		name string
		root string
		want string
	}{
		{
			name: "international",
			root: filepath.Join(home, ".qoder", "projects"),
			want: filepath.Join(appData, "com.qoder.app.stable", QoderTitleDBName),
		},
		{
			name: "cn",
			root: filepath.Join(home, ".qoder-cn", "projects"),
			want: filepath.Join(appData, "com.qodercn.app.stable", QoderTitleDBName),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := QoderTitleDatabase(tc.root)
			assert.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, tc := range []struct {
		name, root string
	}{
		{"qoderwork", filepath.Join(home, ".qoderwork", "projects")},
		{"custom", filepath.Join(home, "sessions", "projects")},
		{"shared client cache", filepath.Join(home, "Library", "Application Support",
			"Qoder", "SharedClientCache", "cli", "projects")},
		{"project directory itself", filepath.Join(home, ".qoder")},
		{"mirror tail match", filepath.Join(home, "mirror-0001", ".qoder", "projects")},
		{"relative", ".qoder/projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, ok := QoderTitleDatabase(tc.root)
			assert.False(t, ok, "only a home-anchored default root may bind a store")
			assert.Empty(t, path)
		})
	}
}

func TestQoderSessionRoot(t *testing.T) {
	home := testjsonl.IsolateQoderHome(t)
	outer := filepath.Join(home, ".qoder", "projects")
	inner := filepath.Join(home, ".qoder-cn", "projects")
	transcript := filepath.Join(inner, "-Users-u-demo", "aaaa.jsonl")
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(inner), 0o755))
	require.NoError(t, os.Symlink(target, inner))
	resolved, _, err := ResolveProviderRoot(AgentQoder, inner)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, path, want string
		ok               bool
	}{
		{"international", filepath.Join(outer, "-Users-u-demo", "aaaa.jsonl"), outer, true},
		{"subagent", filepath.Join(outer, "-Users-u-demo", "aaaa", "subagents", "agent-1.jsonl"), outer, true},
		{"flat file under root", filepath.Join(outer, "aaaa.jsonl"), outer, true},
		{"cn", transcript, resolved, true},
		{"symlinked default root", filepath.Join(resolved, "project", "missing.jsonl"), resolved, true},
		{"no qoder root", filepath.Join("/tmp", "mirror", "projects", "aaaa.jsonl"), "", false},
		{"qoderwork", filepath.Join(home, ".qoderwork", "projects", "aaaa.jsonl"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, ok := QoderSessionRoot(tc.path)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, root)
		})
	}
}

func TestQoderParseTitlePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		database    bool
		row         string
		sidecar     bool
		controls    bool
		rename      bool
		wantName    string
		wantPresent bool
		corrupt     bool
		custom      bool
	}{
		{name: "database beats both files", database: true, row: "From database", sidecar: true, rename: true, wantName: "From database", wantPresent: true},
		{name: "missing database both files", sidecar: true, rename: true, wantName: "From sidecar", wantPresent: true},
		{name: "missing database no title"},
		{name: "custom root no title clears name", custom: true, wantPresent: true},
		{name: "no row sidecar", database: true, sidecar: true, wantName: "From sidecar", wantPresent: true},
		{name: "no row sidecar only controls", database: true, sidecar: true, controls: true, wantName: "\x00\x01\u0080"},
		{name: "blank row sidecar", database: true, row: "   ", sidecar: true, wantName: "From sidecar"},
		{name: "read error sidecar", database: true, sidecar: true, wantName: "From sidecar", corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			if tc.custom {
				root = t.TempDir()
			}
			if tc.database {
				testjsonl.SetQoderTitle(t, store, "other", "Other session")
			}
			if tc.row != "" {
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, tc.row)
			}
			if tc.corrupt {
				require.NoError(t, os.WriteFile(store, []byte("not sqlite"), 0o600))
			}
			path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
			if tc.rename {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				rename := `{"type":"custom-title","sessionId":"` + testjsonl.QoderTitleID + `","customTitle":"Renamed in chat"}` + "\n"
				require.NoError(t, os.WriteFile(path, append(body, []byte(rename)...), 0o644))
			}
			if tc.sidecar {
				body := `{"title":"From sidecar"}`
				if tc.controls {
					body = `{"title":"\u0000\u0001\u0080"}`
				}
				require.NoError(t, os.WriteFile(strings.TrimSuffix(path, ".jsonl")+"-session.json", []byte(body), 0o644))
			}
			results, err := ParseQoderSession(path, "sample-project", "local")
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tc.wantName, results[0].Session.SessionName)
			assert.Equal(t, tc.wantPresent, results[0].Session.SessionNamePresent)
		})
	}
}

// TestQoderTitleTargetsOnlyTheFilesOwnSession covers a transcript that yields
// several sessions: the store row is keyed by the file stem, so only that
// session is titled and a fork keeps its own identity.
func TestQoderTitleTargetsOnlyTheFilesOwnSession(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Own title")
	results := []ParseResult{
		{Session: ParsedSession{Agent: AgentQoder, ID: qoderPrefixID(testjsonl.QoderTitleID)}},
		// A real fork ID is the parent session plus the UUID of the entry that
		// starts the branch, so a prefix match would wrongly title it too.
		{Session: ParsedSession{Agent: AgentQoder, ID: qoderPrefixID(
			testjsonl.QoderTitleID + "-22222222-3333-4333-9333-444444444444",
		)}},
	}
	applyQoderTitles(t.Context(), results, filepath.Join(
		root, testjsonl.QoderProjectDir, testjsonl.QoderTitleID+".jsonl",
	), testjsonl.QoderTitleID)
	assert.Equal(t, "Own title", results[0].Session.SessionName)
	assert.True(t, results[0].Session.SessionNamePresent)
	assert.Empty(t, results[1].Session.SessionName)
	assert.False(t, results[1].Session.SessionNamePresent)
}
