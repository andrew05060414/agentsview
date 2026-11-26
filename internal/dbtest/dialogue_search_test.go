package dbtest

import "testing"

func TestSQLiteDialogueVectorSearch(t *testing.T) {
	d := OpenTestDB(t)
	if !d.HasFTS(t.Context()) {
		t.Skip("fts5 not available")
	}
	SeedDialogueSearchPaging(t, d)
	AssertDialogueVectorSearch(t, d)
}
