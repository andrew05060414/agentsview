//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGFTSBooleanSemantics(t *testing.T) {
	store := setupContentSearch(t)
	for _, row := range []struct{ id, body string }{
		{"a", "reader 压缩 archive"},
		{"b", "reader only"},
		{"c", "库 压缩"},
		{"d", "unrelated"},
		{"e", `reader OR literal NOT "reader"`},
		{"f", "fix bug login"},
		{"g", "fix bug logout"},
		{"h", "a foo"},
		{"i", "b c"},
		{"j", "b only"},
		{"k", "read*"},
		{"l", "read plain *"},
		{"n", "a or b"},
		{"o", "a NEAR b"},
	} {
		insertCSSession(t, store, row.id, "fixture", "codex", "2024-06-15T10:00:00Z", "2024-06-15T11:00:00Z")
		insertCSMessage(t, store, row.id, 0, "user", row.body, "2024-06-15T10:00:00Z", false)
	}
	for _, tc := range []struct {
		query string
		ids   []string
	}{
		{`"reader" OR "压缩"`, []string{"a", "b", "c", "e"}},
		{`"reader"OR"压缩"`, []string{"a", "b", "c", "e"}},
		{`"reader"AND"archive"`, []string{"a"}},
		{`"reader" OR "definitely_absent"`, []string{"a", "b", "e"}},
		{`"reader" NOT "压缩"`, []string{"b", "e"}},
		{`"fix bug" login`, []string{"f"}},
		{`"reader" ""`, []string{"a", "b", "e"}},
		{`""`, nil},
		{`NOT "reader"`, []string{"e"}},
		{`"a" OR "b" "c"`, []string{"a", "b", "d", "e", "h", "i", "k", "l", "n", "o"}},
		{`"b" AND ("only" OR "c")`, []string{"i", "j"}},
		{`"a" or "b"`, []string{"n"}},
		{`"a" NEAR "b"`, []string{"o"}},
		{`"read"*`, []string{"k"}},
		{`read*`, []string{"k"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.query, Mode: "fts", Limit: 50})
			require.NoError(t, err)
			var ids []string
			for _, m := range page.Matches {
				ids = append(ids, m.SessionID)
			}
			assert.Equal(t, tc.ids, ids)
		})
	}
	for _, query := range []string{`"a" OR`, `"a" ("b")`, `"reader" OR broken"`, `"reader" OR ""`} {
		t.Run(query, func(t *testing.T) {
			_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: query, Mode: "fts"})
			var inputErr *db.SearchInputError
			require.ErrorAs(t, err, &inputErr)
			assert.Contains(t, inputErr.Msg, "search: invalid FTS query:")
		})
	}
}
