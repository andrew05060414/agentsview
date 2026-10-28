package db

import (
	"strings"
	"testing"
)

func TestDialogueVisible(t *testing.T) {
	t.Parallel()
	rendering := "[Bash]\n$ ls"
	tests := []struct {
		name      string
		message   Message
		visible   bool
		uncertain bool
	}{
		{
			name:    "plain assistant text",
			message: Message{Role: "assistant", Content: "done"},
			visible: true,
		},
		{
			name:    "plain user text",
			message: Message{Role: "user", Content: "please check"},
			visible: true,
		},
		{
			name:    "tool role excluded",
			message: Message{Role: "tool", Content: "output"},
		},
		{
			name:    "system role excluded",
			message: Message{Role: "system", Content: "notice"},
		},
		{
			name: "tool result subtype excluded",
			message: Message{
				Role: "user", Content: "result text", SourceSubtype: "tool_result",
			},
		},
		{
			name: "pure tool row excluded",
			message: Message{
				Role: "assistant", Content: rendering, HasToolUse: true,
				ToolCalls: []ToolCall{{
					ToolName: "shell", Category: "Bash",
					InputJSON: `{"command":"ls"}`, Rendering: rendering,
				}},
			},
		},
		{
			name: "empty tool row excluded",
			message: Message{
				Role: "assistant", Content: "   ", HasToolUse: true,
				ToolCalls: []ToolCall{{ToolName: "shell", Category: "Bash"}},
			},
		},
		{
			name: "prose plus tool call kept whole",
			message: Message{
				Role: "assistant", Content: "running it\n" + rendering, HasToolUse: true,
				ToolCalls: []ToolCall{{
					ToolName: "shell", Category: "Bash",
					InputJSON: `{"command":"ls"}`, Rendering: rendering,
				}},
			},
			visible: true,
		},
		{
			name: "rendering rebuilt from stored input",
			message: Message{
				Role: "assistant", Content: rendering, HasToolUse: true,
				ToolCalls: []ToolCall{{
					ToolName: "shell", Category: "Bash", InputJSON: `{"command":"ls"}`,
				}},
			},
		},
		{
			name: "missing rendering kept uncertain",
			message: Message{
				Role: "assistant", Content: rendering, HasToolUse: true,
				ToolCalls: []ToolCall{{ToolName: "shell", Category: "Bash"}},
			},
			visible: true, uncertain: true,
		},
		{
			name: "rendering that does not match kept uncertain",
			message: Message{
				Role: "assistant", Content: "see [Bash] below", HasToolUse: true,
				ToolCalls: []ToolCall{{
					ToolName: "shell", Category: "Bash",
					InputJSON: `{"command":"ls"}`, Rendering: rendering,
				}},
			},
			visible: true, uncertain: true,
		},
		{
			name: "tool flag without calls kept uncertain",
			message: Message{
				Role: "assistant", Content: "mentions [Bash] in prose", HasToolUse: true,
			},
			visible: true, uncertain: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DialogueVisible(tt.message)
			if got.Visible != tt.visible || got.Uncertain != tt.uncertain {
				t.Fatalf("DialogueVisible() = %+v, want visible=%v uncertain=%v",
					got, tt.visible, tt.uncertain)
			}
		})
	}
}

// A stored transcript can inline a call's arguments flattened and cut at 220
// runes with an ellipsis. That preview is the whole message, so the row is
// not dialogue.
func TestDialogueVisibleDropsTruncatedArgumentPreview(t *testing.T) {
	t.Parallel()
	args := strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 200)
	flat := strings.Join(strings.Fields(args), " ")
	body := "[Bash]\n" + string([]rune(flat)[:220]) + "..."
	message := Message{
		Role: "assistant", Content: body, HasToolUse: true,
		ToolCalls: []ToolCall{{ToolName: "exec", Category: "Bash", InputJSON: args}},
	}
	if got := DialogueVisible(message); got.Visible || got.Uncertain {
		t.Fatalf("DialogueVisible() = %+v, want the truncated preview dropped", got)
	}
}

func TestDialogueVisibleKeepsOriginalContent(t *testing.T) {
	t.Parallel()
	content := "running it\n[Bash]\n$ ls"
	message := Message{
		Role: "assistant", Content: content, HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "shell", Category: "Bash", InputJSON: `{"command":"ls"}`,
			Rendering: "[Bash]\n$ ls",
		}},
	}
	if got := DialogueVisible(message); !got.Visible || got.Uncertain {
		t.Fatalf("DialogueVisible() = %+v, want the whole message kept", got)
	}
	if message.Content != content {
		t.Fatalf("content rewritten to %q", message.Content)
	}
}
