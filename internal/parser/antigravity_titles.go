package parser

import (
	"context"
	"path/filepath"
)

// AntigravitySummariesDBName is the per-root store where Antigravity keeps
// conversation titles, apart from each conversation's transcript.
const AntigravitySummariesDBName = "conversation_summaries.db"

const antigravityTitleQuery = "SELECT conversation_id, title FROM conversation_summaries"

// Matching markers can hide writes, so cached titles expire.
func AntigravityStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "conversation_summaries", "conversation_id", antigravityTitleQuery)
}

// AntigravitySessionRoot returns the Antigravity root that owns a stored
// conversation database or brain transcript path, even after it was deleted.
func AntigravitySessionRoot(path string) (string, bool) {
	path = filepath.Clean(path)
	if root, _, ok := antigravityBrainTranscriptConversation(path); ok {
		return root, true
	}
	root := filepath.Dir(filepath.Dir(path))
	if _, _, ok := antigravityConversationDBForPath(root, path); ok {
		return root, true
	}
	return "", false
}

// applyAntigravityTitle sets the session's title from its root's store. An
// unreadable store or missing entry leaves no title signal, so the stored
// name survives the write.
func applyAntigravityTitle(ctx context.Context, sess *ParsedSession, src antigravitySource) {
	titles, _, err := AntigravityStoreTitles(ctx, filepath.Join(src.Root, AntigravitySummariesDBName))
	if err != nil {
		return
	}
	if title, ok := titles[src.ID]; ok {
		sess.SessionName = title
		sess.SessionNamePresent = true
	}
}

// PreservesStoredSessionName reports whether a parse carries no title signal
// for an agent whose titles live outside the transcript.
func PreservesStoredSessionName(sess ParsedSession) bool {
	return (sess.Agent == AgentCodex ||
		sess.Agent == AgentAntigravity ||
		sess.Agent == AgentQoder) && !sess.SessionNamePresent
}
