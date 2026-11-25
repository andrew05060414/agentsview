package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestDialogueReproRealArchive reads a local archive read-only and reports,
// without message text, how the dialogue filter treats one reproduced session
// plus a small sample of other agents. It skips when no archive is named, so
// CI is unaffected. Set AGENTS_VIEW_REPRO_DB to the archive path to run it.
func TestDialogueReproRealArchive(t *testing.T) {
	path := os.Getenv("AGENTS_VIEW_REPRO_DB")
	if path == "" {
		t.Skip("AGENTS_VIEW_REPRO_DB not set")
	}
	conn, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_query_only=1")
	require.NoError(t, err, "opening archive")
	t.Cleanup(func() { conn.Close() })

	if target := os.Getenv("AGENTS_VIEW_REPRO_SESSION_ID"); target != "" {
		reportSession(t, conn, target, 35, 43)
	}

	for _, agent := range []string{"claude", "chatgpt", "cursor", "cursor-ide", "antigravity", "antigravity-cli"} {
		var sid string
		err := conn.QueryRowContext(t.Context(),
			`SELECT id FROM sessions WHERE agent = ? AND message_count BETWEEN 8 AND 40 ORDER BY ended_at DESC LIMIT 1`,
			agent).Scan(&sid)
		if err != nil {
			t.Logf("%s: no sample session (%v)", agent, err)
			continue
		}
		reportSession(t, conn, sid, 0, 11)
	}
}

func reportSession(t *testing.T, conn *sql.DB, sid string, from, to int) {
	t.Helper()
	msgs, err := readReproMessages(t.Context(), conn, sid, from, to)
	require.NoError(t, err, "reading sample session")
	var visible, dropped, uncertain int
	fmt.Printf("SESSION %s rows=%d\n", sid, len(msgs))
	for _, m := range msgs {
		v := DialogueVisible(m)
		switch {
		case !v.Visible:
			dropped++
		case v.Uncertain:
			uncertain++
			visible++
		default:
			visible++
		}
		flags := ""
		if m.HasToolUse {
			flags += " tool"
		}
		if m.SourceSubtype != "" {
			flags += " subtype=" + m.SourceSubtype
		}
		fmt.Printf("  ord=%d role=%s len=%d calls=%d%s visible=%v uncertain=%v\n",
			m.Ordinal, m.Role, len(m.Content), len(m.ToolCalls), flags, v.Visible, v.Uncertain)
	}
	fmt.Printf("  TOTAL visible=%d dropped=%d uncertain=%d\n", visible, dropped, uncertain)
}

// readReproMessages loads one ordinal range with its tool calls straight from
// the archive, without the package's schema gate, so an older archive still
// answers. Only the columns the dialogue verdict reads are filled in.
func readReproMessages(ctx context.Context, conn *sql.DB, sid string, from, to int) ([]Message, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id, ordinal, role, content, COALESCE(source_subtype,''), has_tool_use
		FROM messages
		WHERE session_id = ? AND ordinal BETWEEN ? AND ?
		ORDER BY ordinal`, sid, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		m.SessionID = sid
		if err := rows.Scan(&m.ID, &m.Ordinal, &m.Role, &m.Content, &m.SourceSubtype, &m.HasToolUse); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range msgs {
		calls, err := readReproCalls(ctx, conn, msgs[i].ID)
		if err != nil {
			return nil, err
		}
		msgs[i].ToolCalls = calls
	}
	return msgs, nil
}

func readReproCalls(ctx context.Context, conn *sql.DB, messageID int64) ([]ToolCall, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT tool_name, category, COALESCE(input_json,'')
		FROM tool_calls WHERE message_id = ? ORDER BY call_index`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []ToolCall
	for rows.Next() {
		var c ToolCall
		if err := rows.Scan(&c.ToolName, &c.Category, &c.InputJSON); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, rows.Err()
}
