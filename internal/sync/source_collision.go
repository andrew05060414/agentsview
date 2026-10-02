package sync

import (
	"context"
	"log"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// sourceCollisionID returns the raw session id to store s under. When another
// source file the provider still serves owns s.ID, the owner keeps it and
// this file is stored under parser.AltSessionID as a continuation of it, so
// neither file's transcript replaces the other's.
func (e *Engine) sourceCollisionID(
	ctx context.Context,
	provider parser.Provider,
	lookupPath string,
	s *parser.ParsedSession,
) string {
	if !collisionPolicyApplies(provider, s.Agent) {
		return s.ID
	}
	fullID := applyIDPrefixToID(e.idPrefix, s.ID)
	records := e.sessionPathRecords(ctx, fullID)
	stored := ""
	for _, r := range records {
		// A base owner whose source is missing no longer holds the id.
		if r.ID == fullID && !r.SourceMissing {
			stored = r.FilePath
		}
	}
	if stored == lookupPath {
		return s.ID
	}
	altID := e.existingAltID(ctx, provider, records, fullID, s.ID, lookupPath)
	if altID == "" {
		if !e.ownerElsewhere(ctx, provider, stored, lookupPath) &&
			e.claimSessionID(ctx, provider, fullID, lookupPath) {
			return s.ID
		}
		altID = parser.AltSessionID(s.ID, lookupPath)
	}
	if s.ParentSessionID == "" {
		s.ParentSessionID = s.ID
		s.RelationshipType = parser.RelContinuation
	}
	s.ID = altID
	return altID
}

// collisionPolicyApplies excludes providers that rank duplicate copies
// themselves, Claude- and Codex-format agents, which keep their own rules,
// and multi-session containers, whose members never reach the policy.
func collisionPolicyApplies(provider parser.Provider, agent parser.AgentType) bool {
	_, ranks := provider.(parser.ReconciliationSourceRanker)
	return !ranks && !isClaudeFormatAgent(agent) && !isCodexFormatAgent(agent) &&
		provider.Capabilities().Source.MultiSessionSource != parser.CapabilitySupported
}

// collisionPolicyAgents lists the configured agents sourceCollisionID covers.
func (e *Engine) collisionPolicyAgents() []string {
	var agents []string
	for agent, factory := range e.sources().providerFactories {
		if factory != nil && collisionPolicyApplies(factory.NewProvider(parser.ProviderConfig{}), agent) {
			agents = append(agents, string(agent))
		}
	}
	return agents
}

// sessionPathRecords returns the stored and deleted records for fullID and
// its derived ids. During a rebuild it adds the original archive's records
// for ids the new archive has not written yet.
func (e *Engine) sessionPathRecords(ctx context.Context, fullID string) []db.SessionPathRecord {
	records, err := e.db.ListSessionPathRecords(ctx, fullID)
	if err != nil {
		log.Printf("session path records for %s: %v", fullID, err)
	}
	if index := e.archiveStaleClaudeForks; index != nil {
		seen := make(map[string]bool, len(records))
		for _, r := range records {
			seen[r.ID] = true
		}
		for _, r := range index.pathRecords[fullID] {
			if !seen[r.ID] {
				records = append(records, r)
			}
		}
	}
	return records
}

// existingAltID returns the derived id already held by this file, stored or
// deleted, including one the provider has since moved to lookupPath, so its
// curation and any deletion carry over. It returns "" when there is none.
func (e *Engine) existingAltID(
	ctx context.Context, provider parser.Provider, records []db.SessionPathRecord,
	fullID, rawID, lookupPath string,
) string {
	minted := applyIDPrefixToID(e.idPrefix, parser.AltSessionID(rawID, lookupPath))
	for _, r := range records {
		if r.ID != fullID && (r.ID == minted || e.storedSourceLivesAt(ctx, provider, r.FilePath, lookupPath)) {
			return rawID + r.ID[len(fullID):]
		}
	}
	return ""
}

// claimSessionID records path as the owner of an id for this pass. It fails
// when another file earlier in the pass claimed the id and its provider
// still serves that file.
func (e *Engine) claimSessionID(
	ctx context.Context, provider parser.Provider, fullID, path string,
) bool {
	e.sourceClaimsMu.Lock()
	defer e.sourceClaimsMu.Unlock()
	if e.ownerElsewhere(ctx, provider, e.sourceClaims[fullID], path) {
		return false
	}
	if e.sourceClaims == nil {
		e.sourceClaims = make(map[string]string)
	}
	e.sourceClaims[fullID] = path
	return true
}

// ownerElsewhere reports whether the provider still serves the stored source
// path as a file other than the one at path.
func (e *Engine) ownerElsewhere(
	ctx context.Context, provider parser.Provider, stored, path string,
) bool {
	if stored == "" || stored == path {
		return false
	}
	if e.pathRewriter != nil {
		// Rewritten remote paths are never proven gone.
		return true
	}
	at, live := e.providerSourcePath(ctx, provider, stored)
	return live && at != path
}

// storedSourceLivesAt reports whether a stored source path is the file at
// path, or the provider has moved that source there.
func (e *Engine) storedSourceLivesAt(
	ctx context.Context, provider parser.Provider, stored, path string,
) bool {
	if stored == "" || stored == path || e.pathRewriter != nil {
		return stored != "" && stored == path
	}
	at, live := e.providerSourcePath(ctx, provider, stored)
	return live && at == path
}

// providerSourcePath asks the provider where it serves a stored source path
// now. live is false only when the provider proves the source gone.
func (e *Engine) providerSourcePath(
	ctx context.Context, provider parser.Provider, stored string,
) (path string, live bool) {
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		StoredFilePath: stored, RequireFreshSource: true,
	})
	if err != nil {
		return stored, true
	}
	return providerDiscoveredPath(source), found
}
