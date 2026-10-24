package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// Session continuation carries only IDs and the filter fingerprint. Archive
// exclusion happens before LIMIT, using the existing backend contract.
type sessionSearchCursor struct {
	Query  string   `json:"q"`
	IDs    []string `json:"ids"`
	Cutoff string   `json:"cutoff,omitempty"`
}

func decodeSessionSearchCursor(raw string) (sessionSearchCursor, error) {
	var state sessionSearchCursor
	if len(raw) > 131072 {
		return state, errors.New("session_cursor too large")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return state, errors.New("invalid session_cursor")
	}
	if err := json.Unmarshal(b, &state); err != nil || len(state.IDs) > 512 || state.Query == "" {
		return state, errors.New("invalid session_cursor")
	}
	return state, nil
}

func (t *toolset) searchUniqueSessions(ctx context.Context, req service.ContentSearchRequest, state sessionSearchCursor) (*service.ContentSearchResult, string, error) {
	filter := req
	filter.Limit, filter.Cursor = 0, 0
	raw, err := json.Marshal(filter)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(raw)
	query := hex.EncodeToString(digest[:])
	if state.Query != "" && state.Query != query {
		return nil, "", errors.New("session_cursor search filters changed; restart without a cursor")
	}
	if len(state.IDs) >= 512 {
		return nil, "", errors.New("session search continuation limit reached; narrow date or project filters")
	}
	state.Query, state.Cutoff = query, req.ExcludeActiveSince
	seen := make(map[string]bool)
	for _, id := range state.IDs {
		seen[id] = true
	}
	out := &service.ContentSearchResult{Matches: []db.ContentMatch{}}
	limit := req.Limit
	if remaining := 512 - len(state.IDs); limit > remaining {
		limit = remaining
	}
	for range 8 {
		req.ExcludeSessionIDs = db.NormalizeExcludeSessionIDs(append(req.ExcludeSessionIDs, state.IDs...))
		req.Limit, req.Cursor = 50, 0
		page, err := t.svc.SearchContent(ctx, req)
		if err != nil {
			return nil, "", err
		}
		out.ActiveFilterApplied, out.RevisionBound, out.Coverage = page.ActiveFilterApplied, page.RevisionBound, page.Coverage
		added := 0
		for _, match := range page.Matches {
			if seen[match.SessionID] {
				continue
			}
			seen[match.SessionID] = true
			state.IDs = append(state.IDs, match.SessionID)
			out.Matches = append(out.Matches, match)
			added++
			if len(out.Matches) == limit {
				b, err := json.Marshal(state)
				return out, base64.RawURLEncoding.EncodeToString(b), err
			}
		}
		if page.NextCursor == 0 {
			return out, "", nil
		}
		if added == 0 {
			return nil, "", errors.New("backend did not apply session exclusions; use unique_sessions=false")
		}
	}
	b, err := json.Marshal(state)
	return out, base64.RawURLEncoding.EncodeToString(b), err
}
