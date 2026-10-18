package sync

import (
	"context"
	"fmt"
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

// antigravitySummaryUpdate is one session's change from its root's
// conversation_summaries.db. Empty workspace and parent fields leave the
// stored values in place.
type antigravitySummaryUpdate struct {
	name        *string
	updateTitle bool
	cwd         string
	// project is the parser-derived project for cwd, before worktree mappings.
	project  string
	parentID string
}

// deferredAntigravitySummary is a rebuild change for an archived session that
// reaches the replacement only through the orphan copy.
type deferredAntigravitySummary struct {
	stored db.SessionWriteIdentity
	update antigravitySummaryUpdate
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
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredAntigravitySummaries != nil {
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
		targetCwd := session.Cwd
		if cwdChanged {
			targetCwd = summary.Cwd
		}
		if !e.antigravityRootOwns(root, session.FilePath) || !e.cwdFilter.allows(targetCwd) {
			continue
		}
		update := antigravitySummaryUpdate{updateTitle: titleChanged}
		if titleChanged && title != "" {
			update.name = &title
		}
		// Only a changed workspace is rewritten, so a rename alone never
		// replaces the stored project.
		if cwdChanged {
			update.cwd = summary.Cwd
			update.project = summary.Project
		}
		if parentChanged {
			update.parentID = parentID
		}
		if deferred {
			e.deferredAntigravitySummaries[id] = deferredAntigravitySummary{stored: session, update: update}
			continue
		}
		// Trashed sessions take the change too, so restoring one needs no store change.
		if err := e.refreshAntigravitySummary(ctx, e.db, id, session, update); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// refreshAntigravitySummary writes one summary change into target. A changed
// workspace takes its project through the active worktree mappings, as a
// parsed write does, and records a complete project identity observation.
func (e *Engine) refreshAntigravitySummary(
	ctx context.Context,
	target *db.DB,
	id string,
	stored db.SessionWriteIdentity,
	update antigravitySummaryUpdate,
) error {
	refresh := db.NativeSessionSummary{
		SessionName:     update.name,
		UpdateTitle:     update.updateTitle,
		Cwd:             update.cwd,
		ParentSessionID: update.parentID,
	}
	if update.cwd != "" && update.project != "" {
		project := update.project
		if stored.Machine != "" {
			mapped, ok, err := target.ResolveWorktreeProjectMapping(
				ctx, stored.Machine, update.cwd, project,
			)
			if err != nil {
				return fmt.Errorf("resolve worktree project for %s: %w", id, err)
			}
			if ok {
				project = mapped
			}
		}
		refresh.Project = project
		if obs, ok := e.projectIdentityObservation(db.Session{
			ID:        id,
			Project:   project,
			Machine:   stored.Machine,
			Cwd:       update.cwd,
			GitBranch: stored.GitBranch,
		}); ok {
			refresh.IdentityObservation = obs
			refresh.IdentitySnapshotProject = update.project
		}
	}
	return target.RefreshNativeSessionSummary(ctx, id, refresh)
}

// applyDeferredAntigravitySummaries replays rebuild summary changes once the
// orphan copy has placed the archived sessions in target.
func (e *Engine) applyDeferredAntigravitySummaries(
	ctx context.Context,
	target *db.DB,
	deferred map[string]deferredAntigravitySummary,
) error {
	for _, id := range slices.Sorted(maps.Keys(deferred)) {
		change := deferred[id]
		if err := e.refreshAntigravitySummary(ctx, target, id, change.stored, change.update); err != nil {
			return err
		}
	}
	return nil
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
