package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestProjectsPGUsesReadStore(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/archive")
	remote := dbtest.OpenTestDB(t)
	require.NoError(t, remote.UpsertSession(t.Context(), db.Session{ID: "remote", Project: "shared-project", Agent: "codex", UserMessageCount: 1, MessageCount: 1}))
	stub := stubPGReadStore(t, remote)
	out := captureStdout(t, func() {
		_, err := executeCommand(newRootCommand(), "projects", "--pg", "--json")
		require.NoError(t, err)
	})
	projects := decodeCLIJSON[[]db.ProjectInfo](t, out)
	require.Len(t, projects, 1)
	assert.Equal(t, "shared-project", projects[0].Name)
	assert.True(t, stub.Opened)
	assert.True(t, stub.CleanupCalled)
}

func TestFetchHTTPProjects(t *testing.T) {
	var gotAuth string
	var gotQuery url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		assert.Equal(t, "/api/v1/projects", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query()
		writeJSONResponse(w, `{
			"projects": [
				{"name": "alpha", "session_count": 3},
				{"name": "beta", "session_count": 1}
			]
		}`)
	}))
	defer ts.Close()

	projects, err := fetchHTTPProjects(
		t.Context(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"secret-token",
		true,
		true,
	)

	require.NoError(t, err)
	assert.Equal(t, "Bearer secret-token", gotAuth)
	assert.Equal(t, "false", gotQuery.Get("include_one_shot"))
	assert.Equal(t, "false", gotQuery.Get("include_automated"))
	assert.Equal(t, []db.ProjectInfo{
		{Name: "alpha", SessionCount: 3},
		{Name: "beta", SessionCount: 1},
	}, projects)
}

func TestFetchHTTPProjectsTimesOutStalledDaemon(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		<-r.Context().Done()
	}))
	defer ts.Close()

	oldClient := projectsHTTPClient
	projectsHTTPClient = &http.Client{Timeout: 20 * time.Millisecond}
	t.Cleanup(func() { projectsHTTPClient = oldClient })

	_, err := fetchHTTPProjects(
		t.Context(),
		transport{Mode: transportHTTP, URL: ts.URL},
		"",
		false,
		false,
	)

	require.Error(t, err)
}
