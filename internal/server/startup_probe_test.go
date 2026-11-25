package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startupProbeGet issues a GET against the startup probe endpoint on ts and
// returns the response so callers can inspect headers.
func startupProbeGet(
	t *testing.T, ts *httptest.Server, challenge string,
) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, ts.URL+"/_agentsview/startup", nil,
	)
	require.NoError(t, err)
	if challenge != "" {
		req.Header.Set(startupProbeChallengeHeader, challenge)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	return resp
}

// startupProbeStatus issues the GET, closes the body, and returns the status
// code for checks that only care about routing.
func startupProbeStatus(
	t *testing.T, ts *httptest.Server, challenge string,
) int {
	t.Helper()
	resp := startupProbeGet(t, ts, challenge)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestStartupProbeChallengeRequiresEnabledProbe(t *testing.T) {
	s := testServer(t, 0)
	_, _, err := s.StartupProbeChallenge()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup probe is not enabled")
}

// TestStartupProbeEndpointProof exercises the probe over real HTTP: only this
// Server instance can answer the challenge, and the proof validates only
// against the expected value from StartupProbeChallenge.
func TestStartupProbeEndpointProof(t *testing.T) {
	s := testServer(t, 5*time.Second)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// Disabled: the endpoint does not exist for clients.
	assert.Equal(t, http.StatusNotFound, startupProbeStatus(t, ts, ""))

	require.NoError(t, s.EnableStartupProbe())

	// Missing or malformed challenges are still rejected.
	assert.Equal(t, http.StatusNotFound, startupProbeStatus(t, ts, ""))
	assert.Equal(t, http.StatusNotFound,
		startupProbeStatus(t, ts, "not-valid-base64!!"))
	// Well-formed base64 of the wrong length is also rejected.
	assert.Equal(t, http.StatusNotFound,
		startupProbeStatus(t, ts, "aGVsbG8"))

	challenge, expectedProof, err := s.StartupProbeChallenge()
	require.NoError(t, err)
	resp := startupProbeGet(t, ts, challenge)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, expectedProof, resp.Header.Get(startupProbeProofHeader))
	assert.True(t, ValidStartupProbeResponse(resp, expectedProof))
	assert.False(t, ValidStartupProbeResponse(resp, expectedProof+"x"))
	assert.False(t, ValidStartupProbeResponse(resp, ""))
	resp.Body.Close()

	s.DisableStartupProbe()
	assert.Equal(t, http.StatusNotFound,
		startupProbeStatus(t, ts, challenge))
}

// TestValidStartupProbeResponseEmptyExpected reproduces a latent footgun:
// when the expected proof is empty and the response carries no proof header,
// hmac.Equal sees two empty byte strings and reports a match — an "invalid"
// response would pass. waitForBackendReady never hits this today because
// StartupProbeChallenge errors while the probe is disabled, so the bug is
// latent. Fix tracked in PX-1948; unskip this test with the fix.
func TestValidStartupProbeResponseEmptyExpected(t *testing.T) {
	t.Skip("reproduces PX-1948: empty expected proof accepts a missing proof header")
	resp := &http.Response{Header: http.Header{}}
	assert.False(t, ValidStartupProbeResponse(resp, ""),
		"a missing proof must never validate")
}
