package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func newNativeTitleEngine(t *testing.T, archive *db.DB, agent parser.AgentType, roots ...string) *sync.Engine {
	t.Helper()
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: roots}, Machine: "host-a",
	})
	t.Cleanup(engine.Close)
	return engine
}

func nativeStoredTitle(t *testing.T, archive *db.DB, id string) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func TestAntigravityTitlesFollowStore(t *testing.T) {
	for _, mode := range []string{"brain", "database"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			if mode == "database" {
				dbPath := filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
				require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
				copyAntigravityCLITestSchemaTemplate(t, dbPath)
				transcript = dbPath
			}
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)

			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("First"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "a new session takes its saved title")
			before, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new(transcript), before.FilePath)

			dbtest.SetAntigravityTitle(t, root, "Second")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			after, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("Second"), after.SessionName, "a store-only rename updates the title")
			assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
			assert.Equal(t, before.FileHash, after.FileHash, "a rename leaves the transcript fingerprint alone")

			dbtest.SetAntigravityTitle(t, root, "  ")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Nil(t, nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "a blank saved title clears the name")
			for _, tc := range []struct {
				title   string
				want    *string
				updates int
			}{
				{title: "ab", want: new("ab"), updates: 1},
				{title: "a\x00b", want: new("ab"), updates: 0},
				{title: "\x00", want: nil, updates: 1},
				{title: "\x00\x00", want: nil, updates: 0},
			} {
				dbtest.SetAntigravityTitle(t, root, tc.title)
				plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
				require.NoError(t, err)
				result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Equal(t, tc.updates, result.Stats.TitlesUpdated)
				assert.Equal(t, tc.want, nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
			}
			dbtest.SetAntigravityTitle(t, root, "Direct refresh")
			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "antigravity:"+dbtest.AntigravityTitleID))
			assert.Equal(t, new("Direct refresh"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
		})
	}
}

func TestAntigravityTitlesReachSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	dbtest.SetAntigravityTitle(t, root, "Renamed")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
}

func TestAntigravityUnreadableTitleStoreKeepsTitle(t *testing.T) {
	for name, schema := range map[string]string{
		"corrupt":       "",
		"missing_table": "CREATE TABLE other (title TEXT)",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

			data := []byte("not a database")
			if schema != "" {
				incompatible := filepath.Join(t.TempDir(), parser.AntigravitySummariesDBName)
				conn, err := sql.Open("sqlite3", incompatible)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), schema)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				data, err = os.ReadFile(incompatible)
				require.NoError(t, err)
			}
			require.NoError(t, os.WriteFile(store, data, 0o644))
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
			stats := engine.SyncAll(t.Context(), nil)
			if name == "missing_table" {
				assert.Zero(t, stats.Failed)
			} else {
				assert.Equal(t, 1, stats.Failed)
			}
			assert.Equal(t, new("First"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "an unreadable store keeps the stored title")
		})
	}
}

func TestAntigravityFullSyncBackfillsTitles(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	require.NoError(t, newNativeTitleEngine(t, archive, parser.AgentAntigravity, root).SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))

	dbtest.SetAntigravityTitle(t, root, "Saved while closed")
	newNativeTitleEngine(t, archive, parser.AgentAntigravity, root).SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "existing sessions pick up titles without a transcript change")
}

func TestAntigravityTitleStoreRenamesOnlyItsOwnSessions(t *testing.T) {
	owner, other := t.TempDir(), t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, owner, "hello")
	dbtest.SetAntigravityTitle(t, owner, "Owner title")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, owner, other)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := dbtest.SetAntigravityTitle(t, other, "Other root title")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Owner title"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
}

func TestAntigravityRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
	dbtest.SetAntigravityTitle(t, root, "Later")
	engine.ResyncAll(t.Context(), nil)
	assert.Equal(t, new("Later"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "orphan copies must not restore an older title")
}

func TestAntigravityTitleRenamesRespectCwdFilter(t *testing.T) {
	for _, mode := range []string{"applied", "rebuild"} {
		for _, tc := range []struct {
			name string
			want string
		}{
			{name: "excluded", want: "First"},
			{name: "allowed", want: "Later"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				root, claudeRoot, workspace := t.TempDir(), t.TempDir(), t.TempDir()
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := dbtest.SetAntigravityTitle(t, root, "First")
				archive := dbtest.OpenTestDB(t)
				require.NoError(t, newNativeTitleEngine(t, archive, parser.AgentAntigravity, root).SyncPathsContext(t.Context(), []string{transcript}))
				require.NoError(t, archive.UpdateSessionCwd(t.Context(), "antigravity:"+dbtest.AntigravityTitleID, filepath.Join(workspace, tc.name)))
				allowed := filepath.Join(workspace, "allowed")
				engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentAntigravity: {root}, parser.AgentClaude: {claudeRoot},
					},
					Machine: "host-a", IncludeCwdPrefixes: []string{allowed},
				})
				if mode == "rebuild" {
					require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
					dbtest.WriteTestFile(t, filepath.Join(claudeRoot, "project", "live.jsonl"), []byte(fmt.Sprintf(
						`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"live","cwd":%q,"message":{"role":"user","content":"hello"}}`+"\n", allowed)))
				}
				dbtest.SetAntigravityTitle(t, root, "Later")
				if mode == "rebuild" {
					stats := engine.ResyncAll(t.Context(), nil)
					require.False(t, stats.Aborted)
					require.Equal(t, 1, stats.Synced)
				} else {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
				}
				assert.Equal(t, new(tc.want), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
			})
		}
	}
}

func TestAntigravityTrashedSessionRestoresWithCurrentTitle(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "antigravity:" + dbtest.AntigravityTitleID

	require.NoError(t, archive.SoftDeleteSession(t.Context(), id))
	dbtest.SetAntigravityTitle(t, root, "Renamed in trash")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	_, err := archive.RestoreSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in trash"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
}

func TestAntigravitySummariesSyncWorkspaceAndProject(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)

	// Initial sync without summaries
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	sessionID := "antigravity:" + dbtest.AntigravityTitleID
	before, err := archive.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	assert.Empty(t, before.Cwd)
	assert.Empty(t, before.Project)
	assert.Nil(t, before.SessionName)

	// Now create conversation_summaries.db with workspace and title
	store := filepath.Join(root, parser.AntigravitySummariesDBName)
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

	workspaceJSON := `["file:///d%3A/synthetic-workspace"]`
	parentID := "synthetic-parent-123"
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO conversation_summaries (conversation_id, title, workspace_uris, parent_conversation_id)
		 VALUES (?, ?, ?, ?)`,
		dbtest.AntigravityTitleID, "Synthetic Task Title", workspaceJSON, parentID,
	)
	require.NoError(t, err)

	// Sync the summaries store
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	after, err := archive.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, new("Synthetic Task Title"), after.SessionName)
	assert.Equal(t, filepath.Clean(filepath.FromSlash("d:/synthetic-workspace")), after.Cwd)
	assert.Equal(t, "synthetic_workspace", after.Project)
	require.NotNil(t, after.ParentSessionID)
	assert.Equal(t, "antigravity:"+parentID, *after.ParentSessionID)
	assert.Equal(t, "subagent", after.RelationshipType)

	// Verify session_project_identity_snapshots was populated
	var snapProject, snapRoot string
	err = archive.Reader().QueryRow(t.Context(),
		`SELECT project, root_path FROM session_project_identity_snapshots WHERE session_id = ?`,
		sessionID,
	).Scan(&snapProject, &snapRoot)
	require.NoError(t, err)
	assert.Equal(t, "synthetic_workspace", snapProject)
	// The root is stored in its normalized form, as for parsed writes.
	assert.True(t, strings.EqualFold(filepath.Clean(filepath.FromSlash("d:/synthetic-workspace")), filepath.Clean(filepath.FromSlash(snapRoot))), snapRoot)

	// The snapshot is complete, so mirrors and reporting exports publish it.
	publishable, err := archive.ListPublishableSessionProjectIdentitySnapshots(
		t.Context(), []string{sessionID}, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, publishable, 1)
	assert.NotEmpty(t, publishable[0].KeySource)
	assert.NotEmpty(t, publishable[0].Key)
}

// setAntigravitySummary writes a conversation_summaries row with the optional
// workspace and parent columns the Antigravity desktop app records.
func setAntigravitySummary(t *testing.T, root, title, workspaceURIs, parent string) string {
	t.Helper()
	store := filepath.Join(root, parser.AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		title TEXT,
		workspace_uris TEXT NOT NULL DEFAULT '',
		parent_conversation_id TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)
		 ON CONFLICT(conversation_id) DO UPDATE SET title = excluded.title,
		     workspace_uris = excluded.workspace_uris,
		     parent_conversation_id = excluded.parent_conversation_id`,
		dbtest.AntigravityTitleID, title, workspaceURIs, parent,
	)
	require.NoError(t, err)
	return store
}

func antigravityWorkspaceURIs(dir string) string {
	slashed := filepath.ToSlash(dir)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return `["file://` + slashed + `"]`
}

func antigravityStoredSession(t *testing.T, archive *db.DB) *db.Session {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}

func TestAntigravityReparseKeepsWorkspaceWhenSummaryLosesIt(t *testing.T) {
	for _, mode := range []string{"sync", "rebuild"} {
		for _, variant := range []string{"blank", "json-empty-string", "row-missing", "store-unreadable"} {
			t.Run(mode+"/"+variant, func(t *testing.T) {
				root := t.TempDir()
				workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "synthetic-parent")
				archive := dbtest.OpenTestDB(t)
				engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
				before := antigravityStoredSession(t, archive)
				require.Equal(t, filepath.Clean(workspace), before.Cwd)
				require.NotEmpty(t, before.Project)
				require.NotNil(t, before.ParentSessionID)

				switch variant {
				case "blank":
					setAntigravitySummary(t, root, "Title", "", "")
				case "json-empty-string":
					setAntigravitySummary(t, root, "Title", `""`, "")
				case "row-missing":
					conn, err := sql.Open("sqlite3", store)
					require.NoError(t, err)
					_, err = conn.ExecContext(t.Context(), `DELETE FROM conversation_summaries`)
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				case "store-unreadable":
					require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
				}
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
				if mode == "rebuild" {
					stats := engine.ResyncAll(t.Context(), nil)
					require.False(t, stats.Aborted)
				} else {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
				}

				after := antigravityStoredSession(t, archive)
				require.NotNil(t, after.FirstMessage)
				assert.Equal(t, "hello again", *after.FirstMessage, "the transcript was reparsed")
				assert.Equal(t, before.Cwd, after.Cwd)
				assert.Equal(t, before.Project, after.Project)
				require.NotNil(t, after.ParentSessionID)
				assert.Equal(t, "antigravity:synthetic-parent", *after.ParentSessionID)
				assert.Equal(t, "subagent", after.RelationshipType)
			})
		}
	}
}

func TestAntigravityRebuildAppliesSummaryToSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
	setAntigravitySummary(t, root, "Later", antigravityWorkspaceURIs(workspace), "synthetic-parent")
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	after := antigravityStoredSession(t, archive)
	assert.Equal(t, new("Later"), after.SessionName)
	assert.Equal(t, filepath.Clean(workspace), after.Cwd)
	assert.Equal(t, "synthetic_workspace", after.Project)
	require.NotNil(t, after.ParentSessionID)
	assert.Equal(t, "antigravity:synthetic-parent", *after.ParentSessionID)
	assert.Equal(t, "subagent", after.RelationshipType)
}

func TestAntigravitySummaryWorkspaceFollowsWorktreeMapping(t *testing.T) {
	root := t.TempDir()
	mapped := t.TempDir()
	workspace := filepath.Join(mapped, "synthetic-checkout")
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	_, err := archive.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
		Machine: "host-a", PathPrefix: mapped, Project: "configured_project", Enabled: true,
	})
	require.NoError(t, err)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Empty(t, antigravityStoredSession(t, archive).Cwd)

	store := setAntigravitySummary(t, root, "First", antigravityWorkspaceURIs(workspace), "")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session := antigravityStoredSession(t, archive)
	assert.Equal(t, filepath.Clean(workspace), session.Cwd)
	assert.Equal(t, "configured_project", session.Project, "a summary workspace takes the mapped project")

	setAntigravitySummary(t, root, "Renamed", antigravityWorkspaceURIs(workspace), "")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session = antigravityStoredSession(t, archive)
	assert.Equal(t, new("Renamed"), session.SessionName)
	assert.Equal(t, "configured_project", session.Project, "a rename keeps the mapped project")
}

func TestAntigravityKeptWorkspaceKeepsStoredProjectAfterCheckoutIsGone(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped=%v", mapped), func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(t.TempDir(), "synthetic-repo")
			workspace := filepath.Join(repo, "pkg", "sub")
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
			require.NoError(t, os.MkdirAll(workspace, 0o755))
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, "synthetic_repo", antigravityStoredSession(t, archive).Project,
				"the live checkout groups the subdirectory under its repository")

			want := "synthetic_repo"
			if mapped {
				_, err := archive.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
					Machine: "host-a", PathPrefix: repo, Project: "configured_project", Enabled: true,
				})
				require.NoError(t, err)
				want = "configured_project"
			}
			require.NoError(t, os.RemoveAll(repo))
			store := setAntigravitySummary(t, root, "Title", "", "")
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))

			after := antigravityStoredSession(t, archive)
			assert.Equal(t, filepath.Clean(workspace), after.Cwd)
			assert.Equal(t, want, after.Project,
				"a kept workspace keeps its stored project instead of deriving one from a missing checkout")
		})
	}
}

func TestAntigravitySummaryRepairsPlaceholderIdentity(t *testing.T) {
	for _, mode := range []string{"applied", "rebuild-live", "rebuild-orphan"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			id := "antigravity:" + dbtest.AntigravityTitleID
			// An archive that already holds the workspace with only the
			// placeholder snapshot created when the session was first stored.
			require.NoError(t, archive.UpdateSessionCwd(t.Context(), id, workspace))
			publishable, err := archive.ListPublishableSessionProjectIdentitySnapshots(t.Context(), []string{id}, nil, nil)
			require.NoError(t, err)
			require.Empty(t, publishable)

			store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "")
			switch mode {
			case "applied":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			case "rebuild-live", "rebuild-orphan":
				if mode == "rebuild-orphan" {
					require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
					// A rebuild with no live sources refuses to swap.
					dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
						[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
				}
				stats := engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
			}

			assert.Equal(t, filepath.Clean(workspace), antigravityStoredSession(t, archive).Cwd)
			publishable, err = archive.ListPublishableSessionProjectIdentitySnapshots(t.Context(), []string{id}, nil, nil)
			require.NoError(t, err)
			require.Len(t, publishable, 1, "the repaired identity must survive and be publishable")
			assert.NotEmpty(t, publishable[0].Key)
		})
	}
}

func TestAntigravityUsageOnlyArchiveReconcilesWorkspaceAndParent(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs:      map[parser.AgentType][]string{parser.AgentAntigravity: {root}},
		Machine:        "host-a",
		ArchiveContent: config.ArchiveContentUsage,
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Empty(t, antigravityStoredSession(t, archive).Cwd)

	store := setAntigravitySummary(t, root, "Private title", antigravityWorkspaceURIs(workspace), "synthetic-parent")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session := antigravityStoredSession(t, archive)
	assert.Equal(t, filepath.Clean(workspace), session.Cwd)
	assert.Equal(t, "synthetic_workspace", session.Project)
	require.NotNil(t, session.ParentSessionID)
	assert.Equal(t, "antigravity:synthetic-parent", *session.ParentSessionID)
	assert.Nil(t, session.SessionName, "usage-only archives keep no titles")

	// A full pass rereads the store; with nothing changed it writes nothing.
	assert.Zero(t, engine.SyncAll(t.Context(), nil).TitlesUpdated)
}

func TestAntigravityUnchangedWorkspaceKeepsProjectWhenCheckoutIsGone(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped=%v", mapped), func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(t.TempDir(), "synthetic-repo")
			workspace := filepath.Join(repo, "pkg", "sub")
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
			require.NoError(t, os.MkdirAll(workspace, 0o755))
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, "synthetic_repo", antigravityStoredSession(t, archive).Project)

			want := "synthetic_repo"
			if mapped {
				_, err := archive.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
					Machine: "host-a", PathPrefix: repo, Project: "configured_project", Enabled: true,
				})
				require.NoError(t, err)
				want = "configured_project"
			}
			// No Git remote, and the checkout is gone, while the store still
			// names the same workspace.
			require.NoError(t, os.RemoveAll(repo))
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))

			after := antigravityStoredSession(t, archive)
			require.NotNil(t, after.FirstMessage)
			assert.Equal(t, "hello again", *after.FirstMessage)
			assert.Equal(t, filepath.Clean(workspace), after.Cwd)
			assert.Equal(t, want, after.Project,
				"an unresolvable unchanged workspace keeps the stored repository project")
		})
	}
}

func writeSyntheticGitRepo(t *testing.T, dir, remote string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	if remote != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"),
			[]byte("[remote \"origin\"]\nurl = "+remote+"\n"), 0o644))
	}
}

func TestAntigravityWorkspaceChangeReplacesIdentity(t *testing.T) {
	for _, mode := range []string{"sync-store", "sync-store-and-transcript", "rebuild-live", "rebuild-orphan"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			repos := t.TempDir()
			first := filepath.Join(repos, "synthetic-first")
			second := filepath.Join(repos, "synthetic-second")
			writeSyntheticGitRepo(t, first, "https://example.com/team/first.git")
			writeSyntheticGitRepo(t, second, "https://example.com/team/second.git")
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(first), "")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			id := "antigravity:" + dbtest.AntigravityTitleID
			before, err := archive.ListPublishableSessionProjectIdentitySnapshots(t.Context(), []string{id}, nil, nil)
			require.NoError(t, err)
			require.Len(t, before, 1)
			require.Equal(t, "example.com/team/first", before[0].NormalizedRemote)

			if mode == "rebuild-orphan" {
				require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
				// A rebuild with no live sources refuses to swap.
				dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
					[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
			}
			// This pass is the first to see the new workspace.
			store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(second), "")
			switch mode {
			case "sync-store":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			case "sync-store-and-transcript":
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
			default:
				stats := engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
			}

			after := antigravityStoredSession(t, archive)
			assert.Equal(t, filepath.Clean(second), after.Cwd)
			assert.Equal(t, "synthetic_second", after.Project,
				"the old archive's identity must not restore the previous project")
			snapshots, err := archive.ListPublishableSessionProjectIdentitySnapshots(t.Context(), []string{id}, nil, nil)
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			assert.Equal(t, "example.com/team/second", snapshots[0].NormalizedRemote)
			assert.Equal(t, "synthetic_second", snapshots[0].Project)
		})
	}
}

func TestAntigravityCwdFilterKeepsWorkspaceWhenSummaryLosesIt(t *testing.T) {
	for _, mode := range []string{"sync", "rebuild"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			allowed := t.TempDir()
			workspace := filepath.Join(allowed, "synthetic-workspace")
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "")
			archive := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
				AgentDirs:          map[parser.AgentType][]string{parser.AgentAntigravity: {root}},
				Machine:            "host-a",
				IncludeCwdPrefixes: []string{allowed},
			})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, filepath.Clean(workspace), antigravityStoredSession(t, archive).Cwd)

			setAntigravitySummary(t, root, "Title", "", "")
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			if mode == "rebuild" {
				stats := engine.ResyncAll(t.Context(), nil)
				require.False(t, stats.Aborted)
			} else {
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
			}
			after := antigravityStoredSession(t, archive)
			require.NotNil(t, after.FirstMessage)
			assert.Equal(t, "hello again", *after.FirstMessage, "the kept workspace passes the cwd filter")
			assert.Equal(t, filepath.Clean(workspace), after.Cwd)
		})
	}
}
