package mcp

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type sessionPagingService struct {
	service.SessionService
	requests []service.ContentSearchRequest
}

func (s *sessionPagingService) SearchContent(_ context.Context, req service.ContentSearchRequest) (*service.ContentSearchResult, error) {
	s.requests = append(s.requests, req)
	all := []db.ContentMatch{{SessionID: "a", Ordinal: 0}, {SessionID: "a", Ordinal: 1}, {SessionID: "b"}, {SessionID: "c"}}
	var matches []db.ContentMatch
	for _, m := range all {
		if !slices.Contains(req.ExcludeSessionIDs, m.SessionID) {
			matches = append(matches, m)
		}
	}
	return &service.ContentSearchResult{Matches: matches, ActiveFilterApplied: true}, nil
}

func TestMemoryProfileSessionPagination(t *testing.T) {
	svc := &sessionPagingService{}
	ts := &toolset{svc: svc, memoryProfile: true, now: func() time.Time { return fixedNow }}
	in := searchContentIn{Pattern: "archive", Limit: 2}
	_, page, err := ts.searchContent(t.Context(), nil, in)
	require.NoError(t, err)
	require.Len(t, page.Matches, 2)
	assert.Equal(t, "a", page.Matches[0].SessionID)
	assert.Equal(t, "b", page.Matches[1].SessionID)
	assert.Equal(t, "fts", page.EffectiveMode)
	assert.True(t, svc.requests[0].IncludeOneShot)
	assert.False(t, page.Exclusions.OneShot)
	require.NotEmpty(t, page.NextSessionCursor)
	assert.Nil(t, page.NextCursor)
	in.SessionCursor = page.NextSessionCursor
	_, page2, err := ts.searchContent(t.Context(), nil, in)
	require.NoError(t, err)
	require.Len(t, page2.Matches, 1)
	assert.Equal(t, "c", page2.Matches[0].SessionID)
	assert.Empty(t, page2.NextSessionCursor)
	assert.Equal(t, page.ActivityCutoff, page2.ActivityCutoff)
	in.Pattern = "different"
	_, _, err = ts.searchContent(t.Context(), nil, in)
	require.ErrorContains(t, err, "filters changed")
}

func TestMemoryProfileExplicitOneShotAndMessagePaging(t *testing.T) {
	svc := &sessionPagingService{}
	ts := &toolset{svc: svc, memoryProfile: true}
	no := false
	_, _, err := ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "archive", IncludeActive: true, ExcludeOneShot: true, UniqueSessions: &no, Cursor: 3})
	require.NoError(t, err)
	assert.False(t, svc.requests[0].IncludeOneShot)
	assert.Equal(t, 3, svc.requests[0].Cursor)
	_, _, err = ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "archive", Cursor: 1})
	require.ErrorContains(t, err, "session_cursor")
	_, _, err = ts.searchContent(t.Context(), nil, searchContentIn{Pattern: "archive", SessionCursor: "bad!"})
	require.Error(t, err)
}
