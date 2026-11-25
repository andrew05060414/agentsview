package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

// drainedIdleTracker returns an IdleTracker that has already entered its
// draining state, so BeginWork reports false. A 1ns timeout makes nextWait
// drain on the first tick, and Run returns once that happens.
func drainedIdleTracker(t *testing.T) *server.IdleTracker {
	t.Helper()
	//nolint:kennlint // deadlinetest: the drained state under test is the idle deadline expiring; a 1ns budget forces drain on the first tick.
	tracker := server.NewIdleTracker(time.Nanosecond, func() {})
	tracker.Run(t.Context())
	done, ok := tracker.BeginWork()
	require.False(t, ok, "tracker did not drain")
	done()
	return tracker
}

// TestConversationPruneJobSkipsWhileDraining: when the daemon is suspending,
// the job must not touch the archive at all. A closed database proves the
// skip — any prune attempt would surface a real error instead of nil.
func TestConversationPruneJobSkipsWhileDraining(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.Close())

	job := conversationPruneJob(database, drainedIdleTracker(t))
	assert.NoError(t, job.Run(t.Context()))
}

// TestConversationPruneJobSwallowsWriterClosed: while a maintenance pass
// owns the writer, pruning defers to the next tick instead of surfacing
// ErrWriterClosed as a job failure.
func TestConversationPruneJobSwallowsWriterClosed(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	ctx := t.Context()
	// The prune path only runs while conversation export is active.
	_, err := database.EnsureConversationExportInitialized(ctx)
	require.NoError(t, err)
	require.NoError(t, database.CloseWriter())
	t.Cleanup(func() {
		if err := database.ReopenWriter(); err != nil {
			t.Logf("reopen writer: %v", err)
		}
	})

	job := conversationPruneJob(database, nil)
	require.NoError(t, job.Run(ctx))
	assert.True(t, database.WriterClosed(),
		"the job must not clear the maintenance barrier it found")
}

// TestConversationPruneJobReturnsRealErrors: only ErrWriterClosed is
// retried silently; other database failures still fail the job run.
func TestConversationPruneJobReturnsRealErrors(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.Close())

	job := conversationPruneJob(database, nil)
	err := job.Run(t.Context())
	require.Error(t, err)
	assert.NotErrorIs(t, err, db.ErrWriterClosed)
}
