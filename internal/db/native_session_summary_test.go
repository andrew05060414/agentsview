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
