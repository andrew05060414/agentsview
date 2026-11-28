package sync

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/parser"
)

// ReconcileCommitTrailers discovers editor commits independently of source writes.
func (e *Engine) ReconcileCommitTrailers(ctx context.Context) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if e.reconcileCommitTrailersLocked(ctx) > 0 {
		e.emit("sessions")
	}
}

func (e *Engine) reconcileCommitTrailersLocked(ctx context.Context) int {
	return e.reconcileCommitTrailersWithBudget(ctx, 60*time.Second)
}

const trailerBadObjectCooldown = 30 * time.Minute

func (e *Engine) trailerNow() time.Time {
	if e != nil && e.trailerScanNow != nil {
		return e.trailerScanNow()
	}
	return time.Now()
}

func isTrailerBadObjectError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "fatal: bad object") ||
		strings.Contains(msg, "fatal: failed to read object") ||
		strings.Contains(msg, "fatal: loose object") ||
		strings.Contains(msg, "fatal: packed object")
}

// A nonpositive budget permits one repository attempt before yielding.
func (e *Engine) reconcileCommitTrailersWithBudget(ctx context.Context, budget time.Duration) (stored int) {
	if e.ephemeral || e.forceParse || e.db.ArchiveContent().UsageOnly() {
		return
	}
	repositories, err := e.db.TrailerRepositories(ctx)
	if err != nil {
		log.Printf("trailer repositories: %v", err)
		return
	}
	paths := map[string]string{}
	for _, r := range repositories {
		if !e.cwdFilter.allows(r.Cwd) {
			continue
		}
		identity := e.cachedProjectIdentity(r.Machine, e.survivingTrailerCwd(r.Machine, r.Cwd))
		if identity.gitDir == "" {
			continue
		}
		key := identity.repositoryPath
		if key == "" {
			key = identity.rootPath
		}
		paths[key] = identity.rootPath
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	now := e.trailerNow()
	if e.trailerScanAttempts == nil {
		e.trailerScanAttempts = make(map[string]time.Time)
	}
	if e.trailerScanCooldownUntil == nil {
		e.trailerScanCooldownUntil = make(map[string]time.Time)
	}
	order := slices.Sorted(maps.Keys(paths))
	slices.SortStableFunc(order, func(a, b string) int {
		return e.trailerScanAttempts[a].Compare(e.trailerScanAttempts[b])
	})
	for _, repo := range order {
		if ctx.Err() != nil {
			return
		}
		if until, cooling := e.trailerScanCooldownUntil[repo]; cooling && now.Before(until) {
			continue
		}
		// Failed scans also yield their place to repositories still waiting.
		e.trailerScanAttempts[repo] = now
		n, err := e.reconcileRepositoryTrailers(ctx, repo, paths[repo])
		if err != nil {
			if isTrailerBadObjectError(err) {
				e.trailerScanCooldownUntil[repo] = now.Add(trailerBadObjectCooldown)
				log.Printf("commit trailers in %s: %v (backing off for %s)", repo, err, trailerBadObjectCooldown)
			} else {
				log.Printf("commit trailers in %s: %v", repo, err)
			}
		} else {
			delete(e.trailerScanCooldownUntil, repo)
		}
		stored += n
		if time.Since(start) >= budget {
			return
		}
	}
	return stored
}

// survivingTrailerCwd returns the nearest existing ancestor of a session cwd.
// Archived sessions can name temporary subdirectories that were removed, and
// the checkout that contained them still holds the session's commits.
func (e *Engine) survivingTrailerCwd(machine, cwd string) string {
	if !safeLocalAbsolutePath(cwd) {
		return cwd
	}
	for dir := filepath.Clean(cwd); e.mayDiscoverLocalProject(machine, dir); {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return cwd
}

func (e *Engine) reconcileRepositoryTrailers(ctx context.Context, repo, path string) (int, error) {
	gitDir, commonDir, _ := gitDirectoryContext(path, e.mayProbeLocalPath)
	if !e.mayProbeLocalPath(filepath.Join(gitDir, "HEAD")) ||
		!e.mayProbeLocalPath(filepath.Join(gitDir, "commondir")) ||
		!e.mayProbeLocalPath(filepath.Join(commonDir, "config")) {
		return 0, nil
	}
	// A checkout whose worktree record was pruned still has a .git file
	// naming the removed directory. Git cannot open it, and a failed scan
	// every pass would only repeat the same error.
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	commits, err := gitlog.ScanCommitTrailers(ctx, path)
	if err != nil {
		return 0, err
	}
	if !e.cwdFilter.empty() {
		for i := range commits {
			allowed := commits[i].Sessions[:0]
			for _, value := range commits[i].Sessions {
				agent, id, ok := strings.Cut(value, ":")
				def, known := parser.AgentByPrefix(strings.TrimPrefix(agent+":", "claude:"))
				if !ok || !known {
					continue
				}
				session, err := e.db.GetSessionFull(ctx, def.IDPrefix+id)
				if err != nil {
					return 0, err
				}
				if session != nil && e.cwdFilter.allows(session.Cwd) {
					allowed = append(allowed, value)
				}
			}
			commits[i].Sessions = allowed
		}
	}
	return e.db.StoreTrailerCommitLinks(ctx, repo, commits)
}
