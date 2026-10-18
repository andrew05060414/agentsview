package sync

import (
	"context"
	"fmt"
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
	// Usage-only archives keep no titles, but still take workspaces and parents.
	if _, enabled := sources.providerFactories[parser.AgentAntigravity]; !enabled {
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

// antigravitySessionChange is what a store row changes on one stored session:
// its title, and a working directory and parent it did not have yet.
type antigravitySessionChange struct {
	retitle bool
	name    *string
	machine string
	cwd     string
	// project is the parser-derived project for cwd, before worktree mappings.
	project  string
	parentID string
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
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredAntigravityChanges != nil {
		// A rebuild copies archived sessions without sources only after this pass.
		if archived, err = reader.ListSessionWriteIdentitiesByID(ctx, ids); err != nil {
			return 0, err
		}
	}
	usageOnly := e.db.ArchiveContent().UsageOnly()
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
		change := antigravitySessionChange{machine: session.Machine}
		title := db.SanitizeUTF8(summary.Title)
		change.retitle = summary.TitlePresent && !usageOnly &&
			!(session.SessionName != nil && *session.SessionName == title ||
				session.SessionName == nil && title == "")
		if change.retitle && title != "" {
			change.name = &title
		}
		// The store only fills what the session does not have yet.
		cwd := session.Cwd
		if cwd == "" {
			cwd = summary.Cwd
			change.cwd, change.project = summary.Cwd, summary.Project
		}
		if session.ParserParentSessionID == "" && summary.ParentConversationID != "" {
			change.parentID = prefix + summary.ParentConversationID
		}
		if !change.retitle && change.cwd == "" && change.parentID == "" {
			continue
		}
		if !e.antigravityRootOwns(root, session.FilePath) || !e.cwdFilter.allows(cwd) {
			continue
		}
		if deferred {
			e.deferredAntigravityChanges[id] = change
			continue
		}
		// Trashed sessions take the change too, so restoring one needs no store change.
		if err := e.applyAntigravitySessionChange(ctx, e.db, id, change); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// applyAntigravitySessionChange writes change into target. A filled working
// directory takes its project through the active worktree mappings and
// records a complete project identity, as a parsed write does.
func (e *Engine) applyAntigravitySessionChange(
	ctx context.Context, target *db.DB, id string, change antigravitySessionChange,
) error {
	if change.retitle {
		if err := target.RefreshNativeSessionName(ctx, id, change.name); err != nil {
			return err
		}
	}
	fill := db.NativeSessionMetadata{Cwd: change.cwd, ParentSessionID: change.parentID}
	if change.cwd != "" && change.project != "" {
		fill.Project = change.project
		mapped, ok, err := target.ResolveWorktreeProjectMapping(ctx, change.machine, change.cwd, change.project)
		if err != nil {
			return fmt.Errorf("resolve worktree project for %s: %w", id, err)
		}
		if ok {
			fill.Project = mapped
		}
		if obs, ok := e.projectIdentityObservation(db.Session{
			ID: id, Project: fill.Project, Machine: change.machine, Cwd: change.cwd,
		}); ok {
			fill.Identity = obs
			fill.IdentitySnapshotProject = change.project
		}
	}
	return target.FillNativeSessionMetadata(ctx, id, fill)
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
