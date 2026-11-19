package sync

import (
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
