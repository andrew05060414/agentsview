package parser

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteTitleMissingSchemaWarnsOnStateChange(t *testing.T) {
	for i, tc := range []struct {
		missing, schema string
	}{
		{"column conversation_id", "CREATE TABLE conversation_summaries (title TEXT)"},
		{"column title", "CREATE TABLE conversation_summaries (conversation_id TEXT)"},
		{"table conversation_summaries", "CREATE TABLE other_table (value TEXT)"},
	} {
		t.Run(tc.missing, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), AntigravitySummariesDBName)
			conn, err := sql.Open("sqlite3", store)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), tc.schema)
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			now := time.Now()
			previousNow := sqliteTitleNow
			sqliteTitleNow = func() time.Time { return now }
			t.Cleanup(func() { sqliteTitleNow = previousNow })
			titles, state, err := AntigravityStoreTitles(t.Context(), store)
			require.NoError(t, err)
			assert.Empty(t, titles, "an unavailable title schema yields no titles")
			assert.Contains(t, logs.String(), "Warning:")
			assert.Contains(t, logs.String(), store)
			assert.Contains(t, logs.String(), "missing "+tc.missing)
			assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Warning:")))
			if i != 0 {
				return
			}

			logs.Reset()
			for _, refresh := range []string{"cached", "expired", "invalidated"} {
				if refresh == "expired" {
					now = now.Add(SQLiteContainerDigestVerifyInterval)
				}
				if refresh == "invalidated" {
					InvalidateSQLiteTitles(store)
				}
				titles, _, err = AntigravityStoreTitles(t.Context(), store)
				require.NoError(t, err)
				assert.Empty(t, titles)
				assert.Empty(t, logs.String(), refresh)
			}

			require.NoError(t, os.Chtimes(store, now, now))
			titles, changed, err := AntigravityStoreTitles(t.Context(), store)
			require.NoError(t, err)
			require.NotEqual(t, state, changed)
			assert.Empty(t, titles)
			assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Warning:")))
			assert.Contains(t, logs.String(), "missing "+tc.missing)
		})
	}
}
