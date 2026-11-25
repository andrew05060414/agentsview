//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushSessionAccountAndAliases(t *testing.T) {
	ctx := t.Context()
	pgURL := testPGURL(t)
	const schema = "agentsview_session_account_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "chatgpt:one", Project: "chatgpt", Agent: "chatgpt",
		MessageCount: 1, UserMessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}))
	syncer, err := New(ctx, pgURL, schema, local, "installation-a", true, storage.PusherOptions{
		AccountAliases: map[string]string{"chatgpt:0123456789ab": "work"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(ctx))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	account := func() string {
		t.Helper()
		var got string
		require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT account FROM sessions WHERE id = 'chatgpt:one'`).Scan(&got))
		return got
	}
	assert.Empty(t, account())

	// A label filled after the first push reaches the replica on the next one.
	require.NoError(t, local.BackfillSessionAccount(ctx, "chatgpt:0123456789ab", []string{"chatgpt:one"}))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, "chatgpt:0123456789ab", account())

	store := &Store{pg: syncer.pg}
	aliases, err := store.GetAccountAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"chatgpt:0123456789ab": "work"}, aliases)
}
