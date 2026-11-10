package sync

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState)
	for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
		path := filepath.Join(root, parser.AntigravitySummariesDBName)
		if applied, ok := e.appliedNativeTitles[path]; ok {
			states[path] = applied.state
		}
	}
	return states
}

// Mirror snapshots change only on extraction, so persisted states can seed a delta engine.
func (e *Engine) InjectAntigravityTitleStates(states map[string]parser.SQLiteContainerState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	now := time.Now()
	for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
		path := filepath.Join(root, parser.AntigravitySummariesDBName)
		delete(e.appliedNativeTitles, path)
		if state, ok := states[path]; ok {
			e.appliedNativeTitles[path] = appliedNativeTitleStore{state: state, readAt: now}
		}
	}
}

// applyAntigravityStoreSummaries copies workspace metadata (cwd, project,
// parent conversation) from conversation_summaries.db onto stored sessions.
// Titles go through the shared native title pass; this only fills what the
// title pass does not carry. Archived-only sessions are left to the next parse.
func (e *Engine) applyAntigravityStoreSummaries(ctx context.Context, source nativeTitleSource, root, store string) (updated int, err error) {
	parser.InvalidateAntigravitySummaries(store)
	summaries, _, err := parser.AntigravityStoreSummaries(ctx, store)
	if err != nil || len(summaries) == 0 {
		return 0, err
	}
	prefix := e.idPrefix + string(parser.AgentAntigravity) + ":"
	ids := make([]string, 0, len(summaries))
	for id := range maps.Keys(summaries) {
		ids = append(ids, prefix+id)
	}
	slices.Sort(ids)
	stored, err := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		session, ok := stored[id]
		if !ok {
			continue
		}
		summary := summaries[strings.TrimPrefix(id, prefix)]
		parentID := ""
		if summary.ParentConversationID != "" {
			parentID = prefix + summary.ParentConversationID
		}
		cwdChanged := summary.Cwd != "" && session.Cwd != summary.Cwd
		parentChanged := parentID != "" && session.ParentSessionID == ""
		if !cwdChanged && !parentChanged {
			continue
		}
		targetCwd := summary.Cwd
		if targetCwd == "" {
			targetCwd = session.Cwd
		}
		if !e.nativeRootOwns(source, root, session.FilePath) || !e.cwdFilter.allows(targetCwd) {
			continue
		}
		// Trashed sessions take the metadata too, so restoring one needs no store change.
		if err := e.db.RefreshNativeSessionSummary(ctx, id, nil, false, summary.Cwd, summary.Project, parentID); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}
