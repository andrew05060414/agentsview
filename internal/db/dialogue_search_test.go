package db

import (
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
