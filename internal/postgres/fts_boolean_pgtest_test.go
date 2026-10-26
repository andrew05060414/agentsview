//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGFTSBooleanChineseAndPagination(t *testing.T) {
	store := setupContentSearch(t)
	for _, row := range []struct{ id, body string }{
		{"a", "reader 压缩 archive"}, {"b", "reader only"}, {"c", "库 压缩"},
		{"d", "unrelated"}, {"e", "reader OR literal"},
		{"f", "fix bug login"}, {"g", "fix bug logout"},
		{"h", "a foo"}, {"i", "b c"}, {"j", "b only"},
		{"k", "read*"}, {"l", "read plain *"},
		{"m", "a content:y"}, {"n", "a or b"}, {"o", "a NEAR b"},
	} {
		insertCSSession(t, store, row.id, "fixture", "codex", "2024-06-15T10:00:00Z", "2024-06-15T11:00:00Z")
		insertCSMessage(t, store, row.id, 0, "user", row.body, "2024-06-15T10:00:00Z", false)
	}
	for _, tc := range []struct {
		query string
		ids   []string
	}{
		{"reader 压缩", []string{"a"}},
		{`"reader" OR "压缩"`, []string{"a", "b", "c", "e"}},
		{`"reader" OR "definitely_absent"`, []string{"a", "b", "e"}},
		{"库", []string{"c"}},
		{`"reader" NOT "压缩"`, []string{"b", "e"}},
		{`"fix bug" login`, []string{"f"}},
		{`"a" foo`, []string{"h"}},
		{`"a" OR "b" "c"`, []string{"a", "b", "d", "e", "h", "i", "k", "l", "m", "n", "o"}},
		{`"b" AND ("only" OR "c")`, []string{"i", "j"}},
		{`"a" or "b"`, []string{"n"}},
		{`"a" NEAR "b"`, []string{"o"}},
		{`"read"*`, []string{"k"}},
		{`read*`, []string{"k"}},
		{`"a" content:y`, []string{"m"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			f := db.ContentSearchFilter{Pattern: tc.query, Mode: "fts", Limit: 1}
			var ids []string
			for {
				page, err := store.SearchContent(t.Context(), f)
				require.NoError(t, err)
				for _, m := range page.Matches {
					ids = append(ids, m.SessionID)
				}
				if page.NextCursor == 0 {
					break
				}
				f.Cursor = page.NextCursor
			}
			assert.Equal(t, tc.ids, ids)
		})
	}
	for _, query := range []string{`"a" OR`, `NOT "a"`, `("a"`, `"a" ("b")`, `NEAR("a" "b")`} {
		t.Run(query, func(t *testing.T) {
			_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: query, Mode: "fts"})
			var inputErr *db.SearchInputError
			require.ErrorAs(t, err, &inputErr)
			assert.Contains(t, inputErr.Msg, "search: invalid FTS query:")
		})
	}
}

func TestPGFTSUsesDialogueProjection(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "projected", "fixture", "codex", "2024-06-15T10:00:00Z", "2024-06-15T11:00:00Z")
	insertCSMessage(t, store, "projected", 0, "assistant", "visible answer; hidden 库", "2024-06-15T10:00:00Z", false)
	_, err := store.DB().Exec("UPDATE messages SET dialogue_text=$1, thinking_text=$2 WHERE session_id=$3", "visible answer", "hidden 库", "projected")
	require.NoError(t, err)
	for _, pattern := range []string{"库", `"hidden" OR "absent"`} {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: pattern, Mode: "fts", Limit: 5})
		require.NoError(t, err)
		assert.Empty(t, page.Matches, "FTS must preserve the upstream dialogue/thinking boundary")
	}
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "visible", Mode: "fts", Limit: 5})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.NotContains(t, page.Matches[0].Snippet, "hidden")
}
