package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	sessionsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestQoderTitleAfterProjectsMoveAndSourceReload(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentQoder, root)

	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Name before moving projects")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	assert.Equal(t, new("Name before moving projects"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))

	moved := filepath.Join(filepath.Dir(root), "moved-projects")
	require.NoError(t, os.Rename(root, moved))
	require.NoError(t, os.Symlink(moved, root))
	resolved, _, err := parser.ResolveProviderRoot(parser.AgentQoder, root)
	require.NoError(t, err)
	engine.ReconfigureSources(sessionsync.SourceConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {resolved}},
	})
	transcript = testjsonl.WriteQoderTranscript(t, resolved, testjsonl.QoderTitleID, "changed transcript")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Name after moving projects")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	assert.Equal(t, new("Name after moving projects"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
}

// TestQoderDatabaseRenameReachesUnchangedTranscript covers a session imported
// before the IDE titled it, then renamed only in the app database. The
// transcript never changes, so only the full-sync title pass can update it.
func TestQoderDatabaseRenameReachesUnchangedTranscript(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	archive := dbtest.OpenTestDB(t)
	require.NoError(t, newNativeTitleEngine(t, archive, parser.AgentQoder, root).SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))

	for _, title := range []string{"Saved while closed", "Renamed in the IDE"} {
		testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, title)
		stats := newNativeTitleEngine(t, archive, parser.AgentQoder, root).SyncAll(t.Context(), nil)
		assert.Zero(t, stats.Failed)
		assert.Equal(t, 1, stats.TitlesUpdated)
		assert.Equal(t, new(title), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
	}
}

func TestQoderTitleFallbackOnReparse(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Name from export"}`), 0o644))
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Name in the app")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentQoder, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "   ")
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, new("Name in the app"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
	transcript = testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "changed transcript")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	session, err := archive.GetSessionFull(t.Context(), "qoder:"+testjsonl.QoderTitleID)
	require.NoError(t, err)
	assert.Equal(t, new("changed transcript"), session.FirstMessage)
	assert.Equal(t, new("Name in the app"), session.SessionName)
}

func TestQoderMissingRowFollowsSidecarUpdates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		custom bool
	}{
		{name: "default root"},
		{name: "custom root", custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			custom := tc.custom
			if custom {
				root = t.TempDir()
			}
			testjsonl.SetQoderTitle(t, store, "other-session", "Other title")
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			sidecar := strings.TrimSuffix(transcript, ".jsonl") + "-session.json"
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentQoder, root)
			steps := []struct {
				title                   string
				wantDefault, wantCustom *string
				remove                  bool
			}{
				{title: "First", wantDefault: new("First"), wantCustom: new("First")},
				{title: "Later", wantDefault: new("Later"), wantCustom: new("Later")},
				{remove: true, wantDefault: new("Later"), wantCustom: nil},
				{title: "", wantDefault: new("Later"), wantCustom: nil},
			}
			if custom {
				require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"Later"}`), 0o644))
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
				steps = steps[2:]
			}
			for _, tc := range steps {
				if tc.remove {
					require.NoError(t, os.Remove(sidecar))
				} else {
					require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"`+tc.title+`"}`), 0o644))
				}
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
				assert.Zero(t, engine.LastSyncStats().Failed)
				want := tc.wantDefault
				if custom {
					want = tc.wantCustom
				}
				assert.Equal(t, want, nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
			}
			stats := engine.SyncAll(t.Context(), nil)
			assert.Zero(t, stats.Failed)
			if custom {
				assert.Nil(t, nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
			} else {
				assert.Equal(t, new("Later"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
			}
		})
	}
}
