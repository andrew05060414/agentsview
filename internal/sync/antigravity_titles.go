package sync

import (
	"context"
	"path/filepath"
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

// antigravityStoreFill reports the working directory, project, and parent an
// Antigravity store can fill on the sessions it owns. Titles come from the
// shared native title pass; these columns live outside the transcript too, so
// a usage-only archive still takes them.
func antigravityStoreFill(ctx context.Context, store string) (map[string]nativeSessionFill, error) {
	summaries, _, err := parser.AntigravityStoreSummaries(ctx, store)
	if err != nil || len(summaries) == 0 {
		return nil, err
	}
	fills := make(map[string]nativeSessionFill, len(summaries))
	for id, summary := range summaries {
		if summary.Cwd == "" && summary.ParentConversationID == "" {
			continue
		}
		fills[id] = nativeSessionFill{
			cwd:      summary.Cwd,
			project:  summary.Project,
			parentID: summary.ParentConversationID,
		}
	}
	return fills, nil
}
