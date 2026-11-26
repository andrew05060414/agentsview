package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSearchContentSubstringSkipsToolOnlyRows pins the lexical default: a
// needle that lives only in a tool-only assistant row (an empty dialogue
// projection) is not a message hit, while the same needle in prose is.
func TestSearchContentSubstringSkipsToolOnlyRows(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	empty := ""
	require.NoError(t, d.InsertMessages(t.Context(), []Message{
		{
			SessionID: "s1", Ordinal: 0, Role: "assistant",
			Content: "[Bash]\n$ toolonlyneedle", ContentLength: 24, HasToolUse: true,
			DialogueText: &empty,
			ToolCalls:    []ToolCall{{ToolName: "shell", Category: "Bash", InputJSON: `{"command":"toolonlyneedle"}`}},
		},
		{
			SessionID: "s1", Ordinal: 1, Role: "assistant",
			Content: "the toolonlyneedle is discussed here", ContentLength: 36,
			DialogueText: new("the toolonlyneedle is discussed here"),
		},
	}))

	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "toolonlyneedle", Mode: "substring", Sources: []string{"messages"},
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, 1, page.Matches[0].Ordinal)

	// The tool input itself stays searchable, but only when that source is named.
	page, err = d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "toolonlyneedle", Mode: "substring", Sources: []string{"tool_input"},
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "tool_input", page.Matches[0].Location)
}

// TestSearchContentFTSSkipsToolOnlyRows pins the same boundary for the
// tokenized index: the rendered transcript is indexed separately from the
// dialogue projection, and a message search matches the projection.
func TestSearchContentFTSSkipsToolOnlyRows(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS(t.Context()) {
		t.Skip("FTS not available")
	}
	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	empty := ""
	require.NoError(t, d.InsertMessages(t.Context(), []Message{
		{
			SessionID: "s1", Ordinal: 0, Role: "assistant",
			Content: "ftsneedle appears only in the tool row", ContentLength: 38, HasToolUse: true,
			DialogueText: &empty,
		},
		{
			SessionID: "s1", Ordinal: 1, Role: "assistant",
			Content: "ftsneedle appears in the reply", ContentLength: 29,
			DialogueText: new("ftsneedle appears in the reply"),
		},
	}))

	page, err := d.SearchContent(t.Context(), ContentSearchFilter{
		Pattern: "ftsneedle", Mode: "fts",
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, 1, page.Matches[0].Ordinal)
}

func TestSearchContentThinkingWithEmptyDialogue(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "thinking", "proj", func(s *Session) { s.UserMessageCount = 2 })
	insertMessages(t, d,
		Message{SessionID: "thinking", Ordinal: 0, Role: "assistant", Content: "[Thinking] reasoningneedle", ThinkingText: "reasoningneedle", DialogueText: new("")},
		Message{SessionID: "thinking", Ordinal: 1, Role: "assistant", Content: "[Thinking] reasoningneedle [Bash]", ThinkingText: "reasoningneedle", DialogueText: new(""), HasToolUse: true},
	)
	for _, mode := range []string{"substring", "regex"} {
		t.Run(mode, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{
				Pattern: "reasoningneedle", Mode: mode, Sources: []string{"thinking"}, ExcludeSystem: true,
			})
			require.NoError(t, err)
			require.Len(t, page.Matches, 2)
			for _, match := range page.Matches {
				assert.Equal(t, "thinking", match.Location)
			}
		})
	}
}

func TestSearchContentSystemRoleAcrossLexicalPaths(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "system-role", "proj", func(s *Session) { s.UserMessageCount = 2 })
	insertMessages(t, d,
		Message{SessionID: "system-role", Ordinal: 0, Role: "system", Content: "deploymentneedle additional testword"},
		Message{SessionID: "system-role", Ordinal: 1, Role: "assistant", Content: "deploymentneedle additional testword"},
	)
	for _, tc := range []struct{ name, mode, pattern string }{
		{"substring", "substring", "deploymentneedle"},
		{"regex", "regex", "deploymentneedle"},
		{"explicit FTS", "fts", "deploymentneedle OR absentneedle"},
		{"ranked substring", "substring", "deploymentneedle testword"},
		{"ranked FTS", "fts", "deploymentneedle testword"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.mode == "fts" || tc.name == "ranked substring") && !d.HasFTS(t.Context()) {
				t.Skip("FTS not available")
			}
			filter := ContentSearchFilter{Pattern: tc.pattern, Mode: tc.mode, Sources: []string{"messages"}}
			page, err := d.SearchContent(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, page.Matches, 2)
			assert.ElementsMatch(t, []string{"system", "assistant"}, []string{page.Matches[0].Role, page.Matches[1].Role})
			filter.ExcludeSystem = true
			page, err = d.SearchContent(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Equal(t, "assistant", page.Matches[0].Role)
		})
	}
}

func TestSearchContentSkipsWhitespaceOnlyDialogue(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "ws", "proj", func(s *Session) { s.UserMessageCount = 2 })
	insertMessages(t, d,
		Message{SessionID: "ws", Ordinal: 0, Role: "assistant", Content: "wsneedle", DialogueText: new(" \n\t ")},
		Message{SessionID: "ws", Ordinal: 1, Role: "assistant", Content: "answer", DialogueText: new("answer")},
	)
	for _, tc := range []struct {
		name, mode, pattern string
	}{
		{"substring newline", "substring", "\n"},
		{"regex whitespace", "regex", `\s+`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{
				Pattern: tc.pattern, Mode: tc.mode, Sources: []string{"messages"}, DialogueOnly: true,
			})
			require.NoError(t, err)
			assert.Empty(t, page.Matches)
		})
	}
}

func TestDialogueVectorAnchorsDialectSQL(t *testing.T) {
	sentinel := errors.New("stop")
	for _, tc := range []struct {
		name     string
		dialect  QueryDialect
		contains []string
	}{
		{
			name:    "sqlite",
			dialect: SQLiteQueryDialect(),
			contains: []string{
				"m.dialogue_text IS NULL OR LTRIM(m.dialogue_text,",
				"m.is_system = 0",
				SystemPrefixSQL("m.content", "m.role"),
			},
		},
		{
			name:    "postgres",
			dialect: PostgresQueryDialect(),
			contains: []string{
				"m.dialogue_text IS NULL OR LTRIM(m.dialogue_text,",
				"m.is_system = FALSE",
				PostgresSystemPrefixSQL("m.content", "m.role"),
			},
		},
		{
			name:    "clickhouse",
			dialect: ClickHouseQueryDialect(),
			contains: []string{
				"m.dialogue_text IS NULL OR replaceRegexpOne(ifNull(m.dialogue_text, ''),",
				"m.is_system = false",
				ClickHouseSystemPrefixSQL("m.content", "m.role"),
				"SETTINGS short_circuit_function_evaluation = 'force_enable'",
			},
		},
		{
			name:    "duckdb",
			dialect: DuckDBQueryDialect(),
			contains: []string{
				"m.dialogue_text IS NULL OR LTRIM(m.dialogue_text,",
				"m.is_system = FALSE AND m.is_system_prefixed = FALSE",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			lookup := DialogueVectorAnchors(tc.dialect, func(_ context.Context, query string, _ ...any) (*sql.Rows, error) {
				captured = query
				return nil, sentinel
			})
			_, err := lookup(t.Context(), ContentSearchFilter{DialogueOnly: true}, []VectorHit{{SessionID: "s1", Ordinal: 1}})
			require.ErrorIs(t, err, sentinel)
			for _, want := range tc.contains {
				assert.Contains(t, captured, want)
			}
		})
	}
}
