package db

import (
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// DialogueVerdict is one message's dialogue-visibility decision. Uncertain
// means the stored tool-call rendering could not be matched against the
// content, so the message is kept rather than dropped on a guess.
type DialogueVerdict struct {
	Visible   bool
	Uncertain bool
}

// DialogueVisible reports whether a stored message is dialogue. A message is
// dialogue when its role is user or assistant, it is not a tool result, and
// removing its tool-call renderings still leaves text. A message that carries
// a tool call whose rendering is missing or cannot be located is kept and
// reported uncertain: the read path never stores the exact rendering the
// parser inlined, so only a rendering rebuilt from the call's name and input
// that is an exact substring may be removed, and only when removing every one
// of them leaves nothing. The original content is never rewritten here.
func DialogueVisible(m Message) DialogueVerdict {
	if m.Role != "user" && m.Role != "assistant" {
		return DialogueVerdict{}
	}
	if m.SourceSubtype == parser.SourceSubtypeToolResult {
		return DialogueVerdict{}
	}
	if len(m.ToolCalls) == 0 && !m.HasToolUse {
		return DialogueVerdict{Visible: true}
	}
	if strings.TrimSpace(m.Content) == "" {
		return DialogueVerdict{}
	}
	// A tool flag without the calls it refers to leaves nothing to match, so
	// the message cannot be proven empty of dialogue.
	if len(m.ToolCalls) == 0 {
		return DialogueVerdict{Visible: true, Uncertain: true}
	}
	remaining, certain := removeToolRenderings(m.Content, m.ToolCalls)
	if !certain {
		return DialogueVerdict{Visible: true, Uncertain: true}
	}
	if strings.TrimSpace(remaining) == "" {
		return DialogueVerdict{}
	}
	return DialogueVerdict{Visible: true}
}

// removeToolRenderings subtracts every tool call's rendering from content.
// certain is false when any call has no rendering that is an exact substring:
// the caller must then keep the whole message. Renderings are removed longest
// first so a shorter one cannot consume a longer one's prefix.
func removeToolRenderings(content string, calls []ToolCall) (string, bool) {
	renderings := make([]string, 0, len(calls))
	for _, call := range calls {
		rendering := storedToolRendering(content, call)
		if rendering == "" {
			return content, false
		}
		renderings = append(renderings, rendering)
	}
	remaining := content
	for _, rendering := range renderings {
		if strings.Count(remaining, rendering) == 0 {
			return content, false
		}
	}
	order := append([]string(nil), renderings...)
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && len(order[j]) > len(order[j-1]); j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	for _, rendering := range order {
		remaining = strings.ReplaceAll(remaining, rendering, "")
	}
	return remaining, true
}

// storedToolRendering returns the exact text a parser inlined for call when
// that text can be recovered from what the archive stores. A call read back
// from storage has an empty Rendering (it is never persisted), so the
// rendering is rebuilt from the call's name and input and accepted only when
// it occurs verbatim. A candidate that is only a tool label (it carries no
// command or argument) is refused: such a label is also what the renderers
// emit for a call whose input was not stored, and a reader discussing the
// tool by name would otherwise be deleted. A missing or unmatched rendering
// returns "" so the message is kept.
func storedToolRendering(content string, call ToolCall) string {
	if call.Rendering != "" && strings.Contains(content, call.Rendering) &&
		renderingCarriesPayload(call.Rendering) {
		return call.Rendering
	}
	var found string
	for _, pair := range parser.ToolUseRenderingCandidates(
		"", call.Category, call.ToolName, call.InputJSON,
	) {
		if renderingCarriesPayload(pair.Full) && len(pair.Full) > len(found) &&
			strings.Contains(content, pair.Full) {
			found = pair.Full
		}
	}
	if found != "" {
		return found
	}
	// Some stored transcripts inline the call's raw arguments under the tool
	// header and cut them at 220 runes, so the body is a prefix of the stored
	// input rather than the other way round. That prefix is the payload.
	if body, _, split := strings.Cut(content, "\n"); split {
		if payload := truncatedCallPayload(call.InputJSON, content[len(body)+1:]); payload != "" {
			return body + "\n" + payload
		}
	}
	return ""
}

// truncatedCallPayload reports whether body is exactly one line of header
// plus a prefix of the stored arguments that ends at a 220 rune cut. It
// returns that prefix, or "" when the body is anything else.
func truncatedCallPayload(inputJSON, body string) string {
	input := strings.TrimSpace(inputJSON)
	body = strings.TrimRight(body, "\n")
	if input == "" || body == "" {
		return ""
	}
	// The preview flattens the arguments' whitespace into single spaces, cuts
	// the result at 220 runes, and appends "..." when the cut dropped
	// something. The body matches only when it is exactly that preview.
	flat := strings.Join(strings.Fields(input), " ")
	prefix := []rune(flat)
	if len(prefix) > 220 {
		prefix = prefix[:220]
	}
	got := string(prefix)
	if len([]rune(flat)) > 220 {
		got += "..."
	}
	if body == got {
		return body
	}
	return ""
}

// renderingCarriesPayload reports whether a rendering holds more than its
// bracketed tool label. A single-line "[Bash]" or "[Tool: shell]" only names
// the tool — the renderers emit exactly that when a call's input was not
// stored — so removing it cannot prove the message had no dialogue of its
// own. A rendering with a body past its first line does.
func renderingCarriesPayload(rendering string) bool {
	if strings.TrimSpace(rendering) == "" {
		return false
	}
	_, rest, split := strings.Cut(rendering, "\n")
	return split && strings.TrimSpace(rest) != ""
}
