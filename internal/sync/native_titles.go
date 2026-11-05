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

type appliedNativeTitleStore struct {
	state  parser.SQLiteContainerState
	readAt time.Time
}

type nativeTitleRetryContextKey struct{}

type nativeTitleSource struct {
	agent     parser.AgentType
	storePath func(string) (string, bool)
	read      func(context.Context, string) (map[string]string, parser.SQLiteContainerState, error)
	// invalidate expires every cached read of the store.
	invalidate func(string)
	// fill reports metadata beyond titles that the store can fill on stored
	// sessions, for agents whose store carries more than a title.
	fill                func(context.Context, string) (map[string]nativeSessionFill, error)
	sessionRoot         func(string) (string, bool)
	blankClears         bool
	outsideWatchedRoots bool
}

// nativeSessionFill is what a store row can fill on a session that lacks it.
type nativeSessionFill struct {
	cwd string
	// project is the parser-derived project for cwd, before worktree mappings.
	project  string
	parentID string
}

// nativeSessionChange is what a store row changes on one stored session: its
// title, and a working directory and parent it did not have yet.
type nativeSessionChange struct {
	retitle bool
	name    *string
	machine string
	cwd     string
	project string
	// parentID already carries the archive's agent prefix.
	parentID string
}

var nativeTitleSources = []nativeTitleSource{
	{
		agent: parser.AgentAntigravity,
		storePath: func(root string) (string, bool) {
			return filepath.Join(root, parser.AntigravitySummariesDBName), true
		},
		read:                parser.AntigravityStoreTitles,
		invalidate:          parser.InvalidateAntigravityTitles,
		fill:                antigravityStoreFill,
		sessionRoot:         parser.AntigravitySessionRoot,
		blankClears:         true,
		outsideWatchedRoots: false,
	},
	{
		agent:               parser.AgentQoder,
		storePath:           parser.QoderTitleDatabase,
		read:                parser.QoderStoreTitles,
		invalidate:          parser.InvalidateSQLiteTitles,
		sessionRoot:         parser.QoderSessionRoot,
		blankClears:         false,
		outsideWatchedRoots: true,
	},
}

// Reconcile after parsing so an older parsed title cannot win.
func (e *Engine) reconcileNativeTitlesLocked(ctx context.Context, stats *SyncStats) {
	usageOnly := e.db.ArchiveContent().UsageOnly()
	sources := e.sources()
	if e.appliedNativeTitles == nil {
		e.appliedNativeTitles = make(map[string]appliedNativeTitleStore)
	}
	for _, source := range nativeTitleSources {
		if _, enabled := sources.providerFactories[source.agent]; !enabled {
			continue
		}
		// Usage-only archives keep no titles, but a store that also carries
		// workspaces and parents still fills those.
		if usageOnly && source.fill == nil {
			continue
		}
		seen := make(map[string]bool)
		for _, root := range sources.agentDirs[source.agent] {
			if ctx.Err() != nil {
				return
			}
			store, ok := source.storePath(root)
			if !ok || seen[store] {
				continue
			}
			seen[store] = true
			state, exists := parser.StatSQLiteContainerState(store)
			now := time.Now()
			if prior, ok := e.appliedNativeTitles[store]; ok && exists && prior.state == state && now.Sub(prior.readAt) < parser.SQLiteContainerDigestVerifyInterval {
				continue
			}
			delete(e.appliedNativeTitles, store)
			source.invalidate(store)
			titles, state, err := source.read(ctx, store)
			var fills map[string]nativeSessionFill
			if err == nil && source.fill != nil {
				fills, err = source.fill(ctx, store)
			}
			if err == nil {
				var n int
				n, err = e.applyNativeStoreTitles(ctx, source, root, titles, fills, usageOnly)
				stats.TitlesUpdated += n
			}
			if err != nil {
				stats.RecordFailed()
				// Stores outside watched roots are retried by the next sync.
				if retry, ok := ctx.Value(nativeTitleRetryContextKey{}).(*WatchBatch); ok && !source.outsideWatchedRoots {
					retry.Paths = watchAppendUniqueString(retry.Paths, store)
				}
				source.invalidate(store)
				log.Printf("%s titles %s: %v", source.agent, store, err)
				continue
			}
			if state != (parser.SQLiteContainerState{}) {
				e.appliedNativeTitles[store] = appliedNativeTitleStore{state: state, readAt: now}
			}
		}
	}
}

func (e *Engine) invalidateNativeTitleCache(paths []string) {
	if paths == nil {
		clear(e.appliedNativeTitles)
	}
	for _, source := range nativeTitleSources {
		reachable := make(map[string]bool)
		for _, root := range e.sources().agentDirs[source.agent] {
			if store, ok := source.storePath(root); ok {
				reachable[store] = true
			}
		}
		if paths == nil {
			for store := range reachable {
				source.invalidate(store)
			}
			continue
		}
		for _, path := range paths {
			store := strings.TrimSuffix(filepath.Clean(path), "-wal")
			if !reachable[store] {
				continue
			}
			if source.agent == parser.AgentAntigravity {
				if _, ok := parser.SQLiteContainerPathForEvent(filepath.Dir(store), path, parser.AntigravitySummariesDBName, true); !ok {
					continue
				}
			}
			delete(e.appliedNativeTitles, store)
			source.invalidate(store)
		}
	}
}

// recordNativeTitleUpdates folds a title-only pass into the reported sync
// state. Titles change without any session being written, so a pass that only
// renamed sessions still counts as activity.
func (e *Engine) recordNativeTitleUpdates(stats SyncStats) {
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

func (e *Engine) applyNativeStoreTitles(
	ctx context.Context, source nativeTitleSource, root string,
	titles map[string]string, fills map[string]nativeSessionFill, usageOnly bool,
) (updated int, err error) {
	if len(titles) == 0 && len(fills) == 0 {
		return 0, nil
	}
	prefix := e.idPrefix + string(source.agent) + ":"
	ids := make([]string, 0, len(titles)+len(fills))
	for id := range maps.Keys(titles) {
		ids = append(ids, prefix+id)
	}
	for id := range maps.Keys(fills) {
		if _, titled := titles[id]; !titled {
			ids = append(ids, prefix+id)
		}
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
		key := strings.TrimPrefix(id, prefix)
		title, titled := titles[key]
		change := nativeSessionChange{machine: session.Machine}
		change.retitle = titled && !usageOnly &&
			(title != "" || source.blankClears) &&
			!(session.SessionName != nil && *session.SessionName == title ||
				session.SessionName == nil && title == "")
		if change.retitle && title != "" {
			change.name = &title
		}
		// The store only fills what the session does not have yet.
		fill := fills[key]
		cwd := session.Cwd
		if cwd == "" {
			cwd = fill.cwd
			change.cwd, change.project = fill.cwd, fill.project
		}
		if session.ParserParentSessionID == "" && fill.parentID != "" {
			change.parentID = prefix + fill.parentID
		}
		if !change.retitle && change.cwd == "" && change.parentID == "" {
			continue
		}
		if !e.nativeRootOwns(source, root, session.FilePath) || !e.cwdFilter.allows(cwd) {
			continue
		}
		if deferred {
			e.deferredNativeTitles[id] = change
			continue
		}
		// Trashed sessions take the change too, so restoring one needs no store change.
		if err := e.applyNativeSessionChange(ctx, e.db, id, change); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// applyNativeSessionChange writes change into target. A filled working
// directory takes its project through the active worktree mappings and
// records a complete project identity, as a parsed write does.
func (e *Engine) applyNativeSessionChange(
	ctx context.Context, target *db.DB, id string, change nativeSessionChange,
) error {
	if change.retitle {
		if err := target.RefreshNativeSessionName(ctx, id, change.name); err != nil {
			return err
		}
	}
	if change.cwd == "" && change.parentID == "" {
		return nil
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

// nativeRootOwns keeps a store from renaming a same-ID session that
// belongs to another configured root.
func (e *Engine) nativeRootOwns(source nativeTitleSource, root, storedPath string) bool {
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
	owner, ok := source.sessionRoot(storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}
