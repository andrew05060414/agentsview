package sync

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
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

func (e *Engine) applyAntigravityStoreSummaries(ctx context.Context, root string, summaries map[string]parser.AntigravitySummary) (updated int, err error) {
	if len(summaries) == 0 {
		return 0, nil
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
	var archived map[string]db.SessionWriteIdentity
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredNativeTitles != nil {
		// A rebuild copies archived sessions without sources only after this pass.
		if archived, err = reader.ListSessionWriteIdentitiesByID(ctx, ids); err != nil {
			return 0, err
		}
	}
	for _, id := range ids {
		session, ok := stored[id]
		deferred := false
		if !ok {
			session, deferred = archived[id]
		}
		if !ok && !deferred {
			continue
		}
		summary := summaries[strings.TrimPrefix(id, prefix)]
		title := db.SanitizeUTF8(summary.Title)

		titleChanged := summary.TitlePresent && ((session.SessionName != nil && *session.SessionName != title) ||
			(session.SessionName == nil && title != ""))
		cwdChanged := summary.Cwd != "" && session.Cwd != summary.Cwd
		parentID := ""
		if summary.ParentConversationID != "" {
			parentID = prefix + summary.ParentConversationID
		}
		parentChanged := parentID != "" && session.ParentSessionID == ""

		if !titleChanged && !cwdChanged && !parentChanged {
			continue
		}
		targetCwd := summary.Cwd
		if targetCwd == "" {
			targetCwd = session.Cwd
		}
		if !e.antigravityRootOwns(root, session.FilePath) || !e.cwdFilter.allows(targetCwd) {
			continue
		}
		var name *string
		if summary.TitlePresent && title != "" {
			name = &title
		}
		if deferred {
			if summary.TitlePresent {
				e.deferredNativeTitles[id] = name
			}
			continue
		}
		// Trashed sessions take the change too, so restoring one needs no store change.
		if err := e.db.RefreshNativeSessionSummary(ctx, id, name, summary.TitlePresent, summary.Cwd, summary.Project, parentID); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// antigravityRootOwns keeps a store from renaming a same-ID session that
// belongs to another configured root.
func (e *Engine) antigravityRootOwns(root, storedPath string) bool {
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false
		}
		resolved, ok := e.storedPathResolver(storedPath)
		if !ok {
			return false
		}
		storedPath = resolved
	}
	owner, ok := parser.AntigravitySessionRoot(storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}
