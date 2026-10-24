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
}
