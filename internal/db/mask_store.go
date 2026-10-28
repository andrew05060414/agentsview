// ABOUTME: Ingestion-time secret masking (M1): text is masked at the storage
// ABOUTME: projection, so everything derived from the archive is born clean.
package db

import (
	"slices"

	"go.kenn.io/agentsview/internal/mask"
)

// The storage projection (messagesForStorage, ProjectSessionForStoragePolicy,
// sessionForStoragePolicy, toolCallResultUpdatesForStorage) is the single place
// every write path passes through before text reaches SQLite. Masking there
// means the stored rows, the full-text triggers, the vector mirror and the
// PostgreSQL push all see placeholders and never the secret. Signals and secret
// findings keep being computed from the unprojected messages, so a session that
// leaked a secret is still flagged. Masking is deterministic and idempotent, so
// comparing stored rows with freshly parsed (and projected) messages stays
// stable: a second sync of an unchanged source writes nothing.

// maskStored masks one stored text value. changed is false (and out == s) when
// masking is off or nothing matched.
func maskStored(m *mask.Masker, s string) (out string, changed bool) {
	if m == nil || s == "" {
		return s, false
	}
	out, _ = m.Mask(s)
	return out, out != s
}

func maskStoredPtr(m *mask.Masker, p *string) (*string, bool) {
	if p == nil {
		return p, false
	}
	out, changed := maskStored(m, *p)
	if !changed {
		return p, false
	}
	return &out, true
}

// maskSessionForStorage masks the free-text fields of a session row.
func maskSessionForStorage(s Session) Session {
	m := mask.Default()
	if m == nil {
		return s
	}
	s.FirstMessage, _ = maskStoredPtr(m, s.FirstMessage)
	s.DisplayName, _ = maskStoredPtr(m, s.DisplayName)
	s.SessionName, _ = maskStoredPtr(m, s.SessionName)
	return s
}

// maskMessagesForStorage returns msgs with secrets masked. The input slice and
// the tool-call slices it references are never modified (they are still used to
// compute signals and findings); the result shares them when nothing changed.
func maskMessagesForStorage(msgs []Message) []Message {
	m := mask.Default()
	if m == nil || len(msgs) == 0 {
		return msgs
	}
	var out []Message
	for i := range msgs {
		nm, changed := maskMessage(m, msgs[i])
		if !changed {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		out[i] = nm
	}
	if out == nil {
		return msgs
	}
	return out
}

func maskMessage(m *mask.Masker, msg Message) (Message, bool) {
	changed := false
	if c, ok := maskStored(m, msg.Content); ok {
		if msg.ContentLength == len(msg.Content) {
			msg.ContentLength = len(c)
		}
		msg.Content, changed = c, true
	}
	if c, ok := maskStored(m, msg.ThinkingText); ok {
		msg.ThinkingText, changed = c, true
	}
	var calls []ToolCall
	for j := range msg.ToolCalls {
		tc, ok := maskToolCall(m, msg.ToolCalls[j])
		if !ok {
			continue
		}
		if calls == nil {
			calls = slices.Clone(msg.ToolCalls)
		}
		calls[j] = tc
	}
	if calls != nil {
		msg.ToolCalls, changed = calls, true
	}
	return msg, changed
}

func maskToolCall(m *mask.Masker, tc ToolCall) (ToolCall, bool) {
	changed := false
	if c, ok := maskStored(m, tc.InputJSON); ok {
		tc.InputJSON, changed = c, true
	}
	if c, ok := maskStored(m, tc.ResultContent); ok {
		if tc.ResultContentLength == len(tc.ResultContent) {
			tc.ResultContentLength = len(c)
		}
		tc.ResultContent, changed = c, true
	}
	if c, ok := maskStored(m, tc.Rendering); ok {
		tc.Rendering, changed = c, true
	}
	var events []ToolResultEvent
	for k := range tc.ResultEvents {
		ev, ok := maskResultEvent(m, tc.ResultEvents[k])
		if !ok {
			continue
		}
		if events == nil {
			events = slices.Clone(tc.ResultEvents)
		}
		events[k] = ev
	}
	if events != nil {
		tc.ResultEvents, changed = events, true
	}
	return tc, changed
}

func maskResultEvent(m *mask.Masker, ev ToolResultEvent) (ToolResultEvent, bool) {
	c, ok := maskStored(m, ev.Content)
	if !ok {
		return ev, false
	}
	if ev.ContentLength == len(ev.Content) {
		ev.ContentLength = len(c)
	}
	ev.Content = c
	return ev, true
}

// maskToolCallResultUpdates masks late tool results the same way as result
// events carried by newly inserted messages.
func maskToolCallResultUpdates(updates []ToolCallResultUpdate) []ToolCallResultUpdate {
	m := mask.Default()
	if m == nil || len(updates) == 0 {
		return updates
	}
	var out []ToolCallResultUpdate
	for i := range updates {
		var events []ToolResultEvent
		for k := range updates[i].Events {
			ev, ok := maskResultEvent(m, updates[i].Events[k])
			if !ok {
				continue
			}
			if events == nil {
				events = slices.Clone(updates[i].Events)
			}
			events[k] = ev
		}
		if events == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(updates)
		}
		out[i].Events = events
	}
	if out == nil {
		return updates
	}
	return out
}

// MaskText masks a free-text value for storage with the process-wide masker
// (a no-op when masking is switched off). Exported for derived stores that
// write text outside the main storage projection, such as the Codex staging
// scratch database.
func MaskText(s string) string {
	out, _ := maskStored(mask.Default(), s)
	return out
}

// maskRecallEntryForStorage masks the text of a recall entry (title, body,
// trigger and evidence snippets). Entries are written by an external model from
// session text, so a secret that slipped past session masking is caught here.
func maskRecallEntryForStorage(e RecallEntry) RecallEntry {
	m := mask.Default()
	if m == nil {
		return e
	}
	e.Title, _ = maskStored(m, e.Title)
	e.Body, _ = maskStored(m, e.Body)
	e.Trigger, _ = maskStored(m, e.Trigger)
	if len(e.Evidence) > 0 {
		ev := slices.Clone(e.Evidence)
		for i := range ev {
			ev[i].Snippet, _ = maskStored(m, ev[i].Snippet)
		}
		e.Evidence = ev
	}
	return e
}

// MaskMessagesForStorage applies the write boundary's secret masking to msgs.
// Callers that compare freshly parsed messages with stored rows use it so both
// sides are in stored form.
func MaskMessagesForStorage(msgs []Message) []Message { return maskMessagesForStorage(msgs) }
