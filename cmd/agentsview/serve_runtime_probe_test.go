package main

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

// serveProbeTarget starts a real Server on a fresh loopback listener and
// returns the config that points at it plus the serve error channel.
func serveProbeTarget(t *testing.T) (config.Config, *server.Server, chan error) {
	t.Helper()
	listener, err := server.ListenAvailablePort(t.Context(), "127.0.0.1", 0)
	require.NoError(t, err)
	cfg := config.Config{
		Host:    "127.0.0.1",
		Port:    listener.Addr().(*net.TCPAddr).Port,
		DataDir: t.TempDir(),
	}
	srv := server.New(cfg, dbtest.OpenTestDB(t), nil)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	return cfg, srv, serveErr
}

func shutdownProbeTarget(t *testing.T, srv *server.Server, serveErr chan error) {
	t.Helper()
	require.NoError(t, srv.Shutdown(t.Context()))
	require.ErrorIs(t, <-serveErr, http.ErrServerClosed)
}

// TestWaitForBackendReadyProofSuccess: the readiness wait completes once the
// probe endpoint answers with the proof only this Server instance holds.
func TestWaitForBackendReadyProofSuccess(t *testing.T) {
	cfg, srv, serveErr := serveProbeTarget(t)
	require.NoError(t,
		waitForBackendReady(t.Context(), cfg, srv, "", 5*time.Second, serveErr))
	shutdownProbeTarget(t, srv, serveErr)
}

// TestWaitForBackendReadyRejectsForeignInstance: when another process owns
// the port, its probe answer carries a different key. The wait must keep
// failing instead of accepting the foreign instance — that is what keeps the
// bearer token off a socket owned by someone else.
func TestWaitForBackendReadyRejectsForeignInstance(t *testing.T) {
	cfg, owner, serveErr := serveProbeTarget(t)
	// The squatter also runs a probe, so it answers 204 with a proof derived
	// from its own key rather than a plain 404.
	require.NoError(t, owner.EnableStartupProbe())
	require.NoError(t, waitForLocalPort(
		t.Context(), cfg.Host, cfg.Port, 5*time.Second, serveErr))

	waiter := server.New(cfg, dbtest.OpenTestDB(t), nil)
	err := waitForBackendReady(
		t.Context(), cfg, waiter, "", 2*time.Second, serveErr,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid proof")
	shutdownProbeTarget(t, owner, serveErr)
}
