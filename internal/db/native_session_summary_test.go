package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func setFrictionVersion(t *testing.T, d *DB, id string) {
	t.Helper()
	_, err := d.getWriter().Exec(t.Context(),
		`UPDATE sessions SET friction_rules_version = 'friction-v1' WHERE id = ?`, id)
	require.NoError(t, err)
}

func frictionVersion(t *testing.T, d *DB, id string) string {
	t.Helper()
	var version string
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		`SELECT friction_rules_version FROM sessions WHERE id = ?`, id,
	).Scan(&version))
	return version
}

func TestRefreshNativeSessionSummaryWritesWorkspaceIdentityAndParent(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	cwd := filepath.Join(t.TempDir(), "synthetic-repo")
	insertSession(t, d, "child", "before")
	setFrictionVersion(t, d, "child")

	require.NoError(t, d.RefreshNativeSessionSummary(ctx, "child", NativeSessionSummary{
		Cwd:             cwd,
		Project:         "mapped",
		ParentSessionID: "parent",
		IdentityObservation: export.ProjectIdentityObservation{
			SessionID: "child", Project: "mapped", Machine: defaultMachine, RootPath: cwd,
		},
		IdentitySnapshotProject: "synthetic_repo",
	}))

	s := fullSession(t, d, "child")
	assert.Equal(t, cwd, s.Cwd)
	assert.Equal(t, "mapped", s.Project)
	require.NotNil(t, s.ParentSessionID)
	assert.Equal(t, "parent", *s.ParentSessionID)
	assert.Equal(t, "subagent", s.RelationshipType)
	assert.Empty(t, frictionVersion(t, d, "child"), "a new parent invalidates root-only friction results")

	snapshots, err := d.ListPublishableSessionProjectIdentitySnapshots(ctx, []string{"child"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, snapshots, 1, "the workspace snapshot must be publishable")
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath, snapshots[0].KeySource)
	assert.NotEmpty(t, snapshots[0].Key)
	assert.Equal(t, "synthetic_repo", snapshots[0].Project)

	observations, err := d.ListProjectIdentityObservations(ctx, nil)
	require.NoError(t, err)
	var projects []string
	for _, obs := range observations {
		projects = append(projects, obs.Project)
	}
	assert.Contains(t, projects, "mapped")

	// A title-only refresh with no workspace or parent keeps everything else.
	setFrictionVersion(t, d, "child")
	require.NoError(t, d.RefreshNativeSessionSummary(ctx, "child", NativeSessionSummary{
		SessionName: new("Renamed"), UpdateTitle: true,
	}))
	s = fullSession(t, d, "child")
	assert.Equal(t, new("Renamed"), s.SessionName)
	assert.Equal(t, cwd, s.Cwd)
	assert.Equal(t, "mapped", s.Project)
	require.NotNil(t, s.ParentSessionID)
	assert.Equal(t, "parent", *s.ParentSessionID)
	assert.Equal(t, "friction-v1", frictionVersion(t, d, "child"), "an unchanged parent keeps friction results")

	// An existing parent is never replaced, so friction stays current.
	require.NoError(t, d.RefreshNativeSessionSummary(ctx, "child", NativeSessionSummary{
		ParentSessionID: "other-parent",
	}))
	s = fullSession(t, d, "child")
	assert.Equal(t, "parent", *s.ParentSessionID)
	assert.Equal(t, "friction-v1", frictionVersion(t, d, "child"))
}

func TestRefreshNativeSessionSummaryRejectsForeignObservation(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "child", "before")
	err := d.RefreshNativeSessionSummary(t.Context(), "child", NativeSessionSummary{
		Cwd: "/synthetic", Project: "p",
		IdentityObservation: export.ProjectIdentityObservation{
			SessionID: "other", Project: "p", Machine: defaultMachine, RootPath: "/synthetic",
		},
	})
	require.Error(t, err)
	s := fullSession(t, d, "child")
	assert.Equal(t, "before", s.Project)
}

func TestRefreshNativeSessionSummaryIgnoresMissingSession(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.RefreshNativeSessionSummary(t.Context(), "missing", NativeSessionSummary{
		Cwd: "/synthetic", Project: "p",
		IdentityObservation: export.ProjectIdentityObservation{
			SessionID: "missing", Project: "p", Machine: defaultMachine, RootPath: "/synthetic",
		},
	}))
	observations, err := d.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, observations, "no aggregate evidence is published for an absent session")
}

func fullSession(t *testing.T, d *DB, id string) *Session {
	t.Helper()
	s, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

func TestRefreshNativeSessionSummaryIdentityOnlyRepair(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	cwd := filepath.Join(t.TempDir(), "synthetic-repo")
	insertSession(t, d, "child", "synthetic_repo", func(s *Session) { s.Cwd = cwd })
	_, err := d.getWriter().Exec(ctx,
		`UPDATE sessions SET local_modified_at = '2026-01-01T00:00:00.000Z' WHERE id = 'child'`)
	require.NoError(t, err)

	before, err := d.ListSessionWriteIdentitiesByID(ctx, []string{"child"})
	require.NoError(t, err)
	assert.True(t, before["child"].IdentityNeedsRepair, "the insert placeholder has no identity key")
	assert.Equal(t, "synthetic_repo", before["child"].Project)
	assert.Equal(t, "synthetic_repo", before["child"].SourceProject)

	require.NoError(t, d.RefreshNativeSessionSummary(ctx, "child", NativeSessionSummary{
		IdentityObservation: export.ProjectIdentityObservation{
			SessionID: "child", Project: "synthetic_repo", Machine: defaultMachine, RootPath: cwd,
		},
		IdentitySnapshotProject: "synthetic_repo",
	}))

	after, err := d.ListSessionWriteIdentitiesByID(ctx, []string{"child"})
	require.NoError(t, err)
	assert.False(t, after["child"].IdentityNeedsRepair)
	var modified string
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT local_modified_at FROM sessions WHERE id = 'child'`).Scan(&modified))
	assert.Equal(t, "2026-01-01T00:00:00.000Z", modified, "an identity-only repair leaves the session row alone")
}

func TestCopySessionMetadataKeepsFreshIdentityOverEmptyPlaceholder(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	oldPath := filepath.Join(dir, "old.db")
	oldDB, err := Open(ctx, oldPath)
	require.NoError(t, err)
	// The insert trigger leaves a keyless placeholder with no root.
	require.NoError(t, oldDB.UpsertSession(ctx, Session{
		ID: "session-a", Project: "app", Machine: "host", Agent: "antigravity",
	}))
	require.NoError(t, oldDB.Close())

	fresh := testDB(t)
	root := filepath.Join(dir, "workspace")
	require.NoError(t, fresh.UpsertSession(ctx, Session{
		ID: "session-a", Project: "app", Machine: "host", Agent: "antigravity", Cwd: root,
	}))
	require.NoError(t, fresh.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{
		SessionID: "session-a", Project: "app", Machine: "host", RootPath: root,
	}))

	require.NoError(t, fresh.CopySessionMetadataFrom(oldPath))
	publishable, err := fresh.ListPublishableSessionProjectIdentitySnapshots(ctx, []string{"session-a"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, publishable, 1, "an evidence-free placeholder must not replace the fresh identity")
	assert.Equal(t, root, publishable[0].RootPath)
}

func TestRefreshNativeSessionSummaryReplacesIdentityForChangedWorkspace(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	insertSession(t, d, "child", "first", func(s *Session) { s.Cwd = first })
	require.NoError(t, d.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{
		SessionID: "child", Project: "first", Machine: defaultMachine, RootPath: first,
		GitRemote: "https://example.com/team/first.git",
	}))

	require.NoError(t, d.RefreshNativeSessionSummary(ctx, "child", NativeSessionSummary{
		Cwd: second, Project: "second",
		IdentityObservation: export.ProjectIdentityObservation{
			SessionID: "child", Project: "second", Machine: defaultMachine, RootPath: second,
		},
		IdentitySnapshotProject: "second",
		ReplaceIdentitySnapshot: true,
	}))

	snapshots, err := d.ListPublishableSessionProjectIdentitySnapshots(ctx, []string{"child"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "second", snapshots[0].Project)
	assert.Empty(t, snapshots[0].GitRemote, "the old workspace's repository no longer describes the session")
	observations, err := d.ListProjectIdentityObservations(ctx, nil)
	require.NoError(t, err)
	for _, obs := range observations {
		assert.NotEqual(t, "first", obs.Project, "no aggregate evidence remains for the replaced workspace")
	}
}

func observationsByProject(t *testing.T, d *DB) map[string][]export.ProjectIdentityObservation {
	t.Helper()
	observations, err := d.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(t, err)
	byProject := make(map[string][]export.ProjectIdentityObservation)
	for _, obs := range observations {
		byProject[obs.Project] = append(byProject[obs.Project], obs)
	}
	return byProject
}

func TestRefreshNativeSessionSummaryIdentityFollowsStoredProject(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assigned bool
		trashed  bool
	}{
		{name: "assigned project", assigned: true},
		{name: "trashed session", trashed: true},
		{name: "assigned and trashed", assigned: true, trashed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			first := filepath.Join(t.TempDir(), "first")
			second := filepath.Join(t.TempDir(), "second")
			insertSession(t, d, "child", "first", func(s *Session) { s.Cwd = first })
			require.NoError(t, d.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{
				SessionID: "child", Project: "first", Machine: defaultMachine, RootPath: first,
			}))
			stored := "first"
			if tc.assigned {
				_, err := d.AssignSessionProject(ctx, "child", "chosen")
				require.NoError(t, err)
				stored = "chosen"
			}
			if tc.trashed {
				require.NoError(t, d.SoftDeleteSession(ctx, "child"))
			}

			require.NoError(t, d.RefreshNativeSessionSummary(ctx, "child", NativeSessionSummary{
				Cwd: second, Project: "first", // the derived name may equal the stored one
				IdentityObservation: export.ProjectIdentityObservation{
					SessionID: "child", Project: "first", Machine: defaultMachine, RootPath: second,
				},
				IdentitySnapshotProject: "second",
				ReplaceIdentitySnapshot: true,
			}))

			s := fullSession(t, d, "child")
			assert.Equal(t, stored, s.Project)
			assert.Equal(t, second, s.Cwd)
			snapshots, err := d.listSessionProjectIdentitySnapshots(ctx, []string{"child"})
			require.NoError(t, err)
			require.Contains(t, snapshots, "child")
			assert.Equal(t, second, snapshots["child"].RootPath)
			assert.Equal(t, "second", snapshots["child"].Project, "the snapshot keeps its parser-time label")

			byProject := observationsByProject(t, d)
			for project, observations := range byProject {
				for _, obs := range observations {
					assert.NotEqual(t, first, obs.RootPath, "%s still publishes the replaced workspace", project)
				}
			}
			if tc.trashed {
				assert.Empty(t, byProject, "a trashed session publishes no aggregate evidence")
				return
			}
			require.Len(t, byProject[stored], 1, "evidence is published under the stored project")
			assert.Equal(t, second, byProject[stored][0].RootPath)
			for project := range byProject {
				assert.Equal(t, stored, project)
			}
		})
	}
}
