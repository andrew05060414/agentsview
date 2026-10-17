package sync

import (
	"context"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type appliedAntigravityTitles struct {
	state  parser.SQLiteContainerState
	readAt time.Time
}

type antigravityTitleRetryContextKey struct{}

func (e *Engine) invalidateAntigravityTitleCache(paths []string) {
	if paths == nil {
		clear(e.appliedAntigravityTitles)
		for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
			parser.InvalidateAntigravityTitles(filepath.Join(root, parser.AntigravitySummariesDBName))
		}
	}
	for _, path := range paths {
		store := strings.TrimSuffix(filepath.Clean(path), "-wal")
		if filepath.Base(store) == parser.AntigravitySummariesDBName {
			if _, ok := parser.SQLiteContainerPathForEvent(filepath.Dir(store), path, parser.AntigravitySummariesDBName, true); !ok {
				continue
			}
		}
		delete(e.appliedAntigravityTitles, store)
		parser.InvalidateAntigravityTitles(store)
	}
}

// Reconcile after parsing so an older parsed title cannot win.
func (e *Engine) reconcileAntigravityTitlesLocked(ctx context.Context, stats *SyncStats) {
	sources := e.sources()
	if _, enabled := sources.providerFactories[parser.AgentAntigravity]; !enabled || e.db.ArchiveContent().UsageOnly() {
		return
	}
	if e.appliedAntigravityTitles == nil {
		e.appliedAntigravityTitles = make(map[string]appliedAntigravityTitles)
	}
	for _, root := range sources.agentDirs[parser.AgentAntigravity] {
		if ctx.Err() != nil {
			return
		}
		store := filepath.Join(root, parser.AntigravitySummariesDBName)
		state, exists := parser.StatSQLiteContainerState(store)
		now := time.Now()
		if prior, applied := e.appliedAntigravityTitles[store]; applied && exists && prior.state == state && now.Sub(prior.readAt) < parser.SQLiteContainerDigestVerifyInterval {
			continue
		}
		delete(e.appliedAntigravityTitles, store)
		parser.InvalidateAntigravityTitles(store)
		summaries, state, err := parser.AntigravityStoreSummaries(ctx, store)
		if err == nil {
			var n int
			n, err = e.applyAntigravityStoreSummaries(ctx, root, summaries)
			stats.TitlesUpdated += n
		}
		if err != nil {
			stats.RecordFailed()
			if retry, ok := ctx.Value(antigravityTitleRetryContextKey{}).(*WatchBatch); ok {
				retry.Paths = watchAppendUniqueString(retry.Paths, store)
			}
			parser.InvalidateAntigravityTitles(store)
			log.Printf("antigravity titles %s: %v", store, err)
			continue
		}
		if state != (parser.SQLiteContainerState{}) {
			e.appliedAntigravityTitles[store] = appliedAntigravityTitles{state: state, readAt: now}
		}
	}
}

func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState)
	for path, applied := range e.appliedAntigravityTitles {
		states[path] = applied.state
	}
	return states
}

// Mirror snapshots change only on extraction, so persisted states can seed a delta engine.
func (e *Engine) InjectAntigravityTitleStates(states map[string]parser.SQLiteContainerState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.appliedAntigravityTitles = make(map[string]appliedAntigravityTitles, len(states))
	now := time.Now()
	for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
		path := filepath.Join(root, parser.AntigravitySummariesDBName)
		if state, ok := states[path]; ok {
			e.appliedAntigravityTitles[path] = appliedAntigravityTitles{state: state, readAt: now}
		}
	}
}

func (e *Engine) recordAntigravityTitleUpdates(stats SyncStats) {
	if stats.TitlesUpdated > 0 || stats.Failed > 0 {
		e.mu.Lock()
		if stats.TitlesUpdated > 0 && stats.ProcessingComplete() {
			e.lastSync = time.Now()
		}
		e.lastSyncStats.TitlesUpdated += stats.TitlesUpdated
		e.lastSyncStats.Failed += stats.Failed
		e.mu.Unlock()
	}
}

func (e *Engine) applyAntigravityStoreTitles(ctx context.Context, root string, titles map[string]string) (updated int, err error) {
	summaries := make(map[string]parser.AntigravitySummary, len(titles))
	for id, title := range titles {
		summaries[id] = parser.AntigravitySummary{Title: title, TitlePresent: true}
	}
	return e.applyAntigravityStoreSummaries(ctx, root, summaries)
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
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredAntigravityTitles != nil {
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
		rawID := strings.TrimPrefix(id, prefix)
		summary := summaries[rawID]
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
				e.deferredAntigravityTitles[id] = name
			}
			continue
		}
		// Trashed sessions take the title too, so restoring one needs no store change.
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
