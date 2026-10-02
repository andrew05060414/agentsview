package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"

	"go.kenn.io/agentsview/internal/parser"
)

// sourceCollisionID returns the raw session id to store s under. When another
// source file still on disk owns s.ID, the owner keeps it and this file is
// stored under parser.AltSessionID as a continuation of it, so neither file's
// transcript replaces the other's. Providers that rank duplicate copies
// themselves, and Claude- and Codex-format agents, keep their own rules.
func (e *Engine) sourceCollisionID(
	ctx context.Context,
	provider parser.Provider,
	lookupPath string,
	s *parser.ParsedSession,
) string {
	if _, ranks := provider.(parser.ReconciliationSourceRanker); ranks ||
		isClaudeFormatAgent(s.Agent) || isCodexFormatAgent(s.Agent) {
		return s.ID
	}
	fullID := applyIDPrefixToID(e.idPrefix, s.ID)
	stored := e.db.GetSessionFilePathNotSourceMissing(ctx, fullID)
	if index := e.archiveStaleClaudeForks; stored == "" && index != nil {
		stored = index.altPaths[fullID]
	}
	if stored == lookupPath {
		return s.ID
	}
	altID := parser.AltSessionID(s.ID, lookupPath)
	if !e.altSessionKnown(ctx, applyIDPrefixToID(e.idPrefix, altID)) &&
		!e.sourceFileElsewhere(stored, lookupPath) &&
		e.claimSessionID(fullID, lookupPath) {
		return s.ID
	}
	if s.ParentSessionID == "" {
		s.ParentSessionID = s.ID
		s.RelationshipType = parser.RelContinuation
	}
	s.ID = altID
	return altID
}

// altSessionKnown reports whether this file was already stored under its
// derived id, or the user deleted that session. Either way it keeps the id,
// so a base owner that is later deleted or goes missing is never overwritten.
func (e *Engine) altSessionKnown(ctx context.Context, fullAltID string) bool {
	if index := e.archiveStaleClaudeForks; index != nil {
		if _, ok := index.altPaths[fullAltID]; ok {
			return true
		}
	}
	return e.db.GetSessionFilePath(ctx, fullAltID) != "" ||
		e.db.IsSessionExcluded(ctx, fullAltID)
}

// claimSessionID records path as the owner of an id that no other existing
// file owns in storage. It fails when another file earlier in this pass
// claimed the id and is still on disk.
func (e *Engine) claimSessionID(fullID, path string) bool {
	e.sourceClaimsMu.Lock()
	defer e.sourceClaimsMu.Unlock()
	if e.sourceFileElsewhere(e.sourceClaims[fullID], path) {
		return false
	}
	if e.sourceClaims == nil {
		e.sourceClaims = make(map[string]string)
	}
	e.sourceClaims[fullID] = path
	return true
}

// sourceFileElsewhere reports whether the stored source path names a file
// other than the one at path that may still exist. Only a not-exist stat
// proves it gone; a rewritten path without a local mirror counts as present.
func (e *Engine) sourceFileElsewhere(stored, path string) bool {
	if stored == "" || stored == path {
		return false
	}
	stored, path = e.physicalSourcePath(stored), e.physicalSourcePath(path)
	if stored == "" {
		return true
	}
	info, err := os.Stat(stored)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false
	}
	other, otherErr := os.Stat(path)
	return err != nil || otherErr != nil || !os.SameFile(info, other)
}

// physicalSourcePath maps a stored source path to one this process can stat,
// or "" when a rewritten path has no local mirror.
func (e *Engine) physicalSourcePath(path string) string {
	if e.pathRewriter == nil {
		return path
	}
	if e.storedPathResolver == nil {
		return ""
	}
	physical, ok := e.storedPathResolver(path)
	if !ok {
		return ""
	}
	return physical
}
