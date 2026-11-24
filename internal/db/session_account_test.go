package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionAccountSurvivesOrphanCopy(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "old.db")
	src := testDBAtPath(t, srcPath, "src")
	insertSession(t, src, "chatgpt:one", "chatgpt", func(s *Session) { s.Agent = "chatgpt" })
	require.NoError(t, src.BackfillSessionAccount(t.Context(), "chatgpt:0123456789ab", []string{"chatgpt:one"}))
	src.Close()

	dst := testDBAtPath(t, filepath.Join(dir, "new.db"), "dst")
	defer dst.Close()
	ids, err := dst.CopyOrphanedDataFromExcluding(t.Context(), srcPath, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"chatgpt:one"}, ids)
	copied, err := dst.GetSessionFull(t.Context(), "chatgpt:one")
	require.NoError(t, err)
	require.NotNil(t, copied)
	assert.Equal(t, "chatgpt:0123456789ab", copied.Account, "a resync keeps the account label")
}

func TestListSessionsFiltersByAccount(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"chatgpt:a", "chatgpt:b", "chatgpt:none"} {
		insertSession(t, d, id, "chatgpt", func(s *Session) { s.Agent = "chatgpt" })
	}
	require.NoError(t, d.BackfillSessionAccount(t.Context(), "chatgpt:00000000000a", []string{"chatgpt:a"}))
	require.NoError(t, d.BackfillSessionAccount(t.Context(), "chatgpt:00000000000b", []string{"chatgpt:b"}))

	page, err := d.ListSessions(t.Context(), SessionFilter{Account: "chatgpt:00000000000a", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "chatgpt:a", page.Sessions[0].ID)
	assert.Equal(t, "chatgpt:00000000000a", page.Sessions[0].Account)

	page, err = d.ListSessions(t.Context(), SessionFilter{Account: "chatgpt:00000000000a,chatgpt:00000000000b", Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 2)
}
