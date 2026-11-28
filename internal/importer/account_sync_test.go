package importer

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

var geminiSlotPattern = regexp.MustCompile(`^/u/([0-9])(/.*)$`)

// withoutAccount lets fakes written before account labels ignore the identity
// requests: every identity reply is empty, and Gemini slot 0 paths are
// unprefixed so the fake sees the paths it was written for.
func withoutAccount(fetch BrowserFetch) BrowserFetch {
	return func(ctx context.Context, path string) (BrowserResponse, error) {
		if slot := geminiSlotPattern.FindStringSubmatch(path); slot != nil {
			if slot[2] == geminiRequest(geminiAccountRequest, "", "") {
				return accountReply(""), nil
			}
			path = slot[2]
		}
		switch path {
		case browserRequest(chatGPTRequests, chatGPTAccountRequest), browserRequest(grokWebRequests, grokAccountRequest):
			return accountReply(""), nil
		}
		return fetch(ctx, path)
	}
}

func syncChatGPTTest(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (ImportStats, error) {
	return syncChatGPT(ctx, store, withoutAccount(fetch), opts, cb)
}

func syncGrokTest(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (ImportStats, error) {
	return syncGrokWeb(ctx, store, withoutAccount(fetch), opts, cb)
}

func syncGeminiTest(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (ImportStats, error) {
	return syncGeminiApps(ctx, store, withoutAccount(fetch), opts, cb)
}

// accountReply is the extension's answer to an identity request: the first
// 12 hex digits of the identity's SHA-256, or empty when unknown.
func accountReply(account string) BrowserResponse {
	return BrowserResponse{Status: 200, Body: []byte(`{"account":"` + account + `"}`)}
}

func sessionAccount(t *testing.T, d *db.DB, id string) string {
	t.Helper()
	session, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session, id)
	return session.Account
}

func TestMigratedSessionsHaveNoAccount(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "old", Project: "p", Agent: "claude"}))
	assert.Empty(t, sessionAccount(t, d, "old"))
}

func TestBackfillSessionAccountKeepsExistingLabel(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "a", Project: "p", Agent: "chatgpt"}))
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "b", Project: "p", Agent: "chatgpt"}))
	require.NoError(t, d.BackfillSessionAccount(ctx, "chatgpt:aaaaaaaaaaaa", []string{"a"}))
	before, err := d.GetSessionFull(ctx, "b")
	require.NoError(t, err)
	require.NoError(t, d.BackfillSessionAccount(ctx, "chatgpt:bbbbbbbbbbbb", []string{"a", "b"}))
	assert.Equal(t, "chatgpt:aaaaaaaaaaaa", sessionAccount(t, d, "a"), "a set label is never replaced")
	after, err := d.GetSessionFull(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, "chatgpt:bbbbbbbbbbbb", after.Account)
	require.NotNil(t, after.LocalModifiedAt)
	if before.LocalModifiedAt != nil {
		assert.NotEqual(t, *before.LocalModifiedAt, *after.LocalModifiedAt, "a filled label must reach the next push")
	}

	// A later upsert from a parser, which knows no account, keeps the label.
	require.NoError(t, d.UpsertSession(ctx, db.Session{ID: "a", Project: "p", Agent: "chatgpt"}))
	assert.Equal(t, "chatgpt:aaaaaaaaaaaa", sessionAccount(t, d, "a"))
}

// chatGPTAccountFetch serves one conversation and answers identity requests
// from accounts in order, repeating the last one.
func chatGPTAccountFetch(t *testing.T, id string, accounts ...BrowserResponse) BrowserFetch {
	asked := 0
	return func(_ context.Context, path string) (BrowserResponse, error) {
		switch path {
		case browserRequest(chatGPTRequests, chatGPTAccountRequest):
			reply := accounts[min(asked, len(accounts)-1)]
			asked++
			return reply, nil
		case chatGPTRequest(chatGPTListRequest, "", 0, true):
			return BrowserResponse{Status: 200, Body: []byte(`{"items":[],"total":0}`)}, nil
		case chatGPTRequest(chatGPTListRequest, "", 0, false):
			return BrowserResponse{Status: 200, Body: []byte(fmt.Sprintf(`{"items":[{"id":%q,"update_time":1}],"total":1}`, id))}, nil
		case chatGPTRequest(chatGPTDetailRequest, id, 0, false):
			return BrowserResponse{Status: 200, Body: []byte(strings.ReplaceAll(chatGPTSyncDetail(1, false, "Question"), chatGPTSyncID, id))}, nil
		}
		require.Failf(t, "unexpected ChatGPT request path", "%s", path)
		return BrowserResponse{}, nil
	}
}

func TestSyncChatGPTRecordsAccount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []BrowserResponse
		want     string
	}{
		{name: "same account at both ends", accounts: []BrowserResponse{accountReply("0123456789ab")}, want: "chatgpt:0123456789ab"},
		{name: "account switched during the round", accounts: []BrowserResponse{accountReply("0123456789ab"), accountReply("ba9876543210")}},
		{name: "identity unavailable", accounts: []BrowserResponse{{Status: 404}}},
		{name: "identity not recognized", accounts: []BrowserResponse{accountReply("")}},
		{name: "malformed identity", accounts: []BrowserResponse{accountReply("not-a-hash!!")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			stats, err := syncChatGPT(t.Context(), d, chatGPTAccountFetch(t, chatGPTSyncID, tc.accounts...), BrowserSyncOptions{}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Imported, "the conversation syncs whatever the identity")
			assert.Equal(t, tc.want, sessionAccount(t, d, "chatgpt:"+chatGPTSyncID))
		})
	}
}

func TestSyncChatGPTBackfillsUnlabelledSession(t *testing.T) {
	d := testDB(t)
	_, err := syncChatGPT(t.Context(), d, chatGPTAccountFetch(t, chatGPTSyncID, BrowserResponse{Status: 404}), BrowserSyncOptions{}, nil)
	require.NoError(t, err)
	require.Empty(t, sessionAccount(t, d, "chatgpt:"+chatGPTSyncID))

	stats, err := syncChatGPT(t.Context(), d, chatGPTAccountFetch(t, chatGPTSyncID, accountReply("0123456789ab")), BrowserSyncOptions{}, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Imported+stats.Updated, "the unchanged conversation is not fetched again")
	assert.Equal(t, "chatgpt:0123456789ab", sessionAccount(t, d, "chatgpt:"+chatGPTSyncID))

	_, err = syncChatGPT(t.Context(), d, chatGPTAccountFetch(t, chatGPTSyncID, accountReply("ba9876543210")), BrowserSyncOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "chatgpt:0123456789ab", sessionAccount(t, d, "chatgpt:"+chatGPTSyncID), "a set label is never replaced")
}

func TestSyncGrokRecordsAccount(t *testing.T) {
	d := testDB(t)
	fake := &grokFake{t: t, modified: "2026-10-21T08:00:00Z", responses: []map[string]any{grokSyncResponse("00000000-0000-4000-8000-0000000000a1", "", "human", "Hi", "2026-10-21T08:00:00Z")}}
	fetch := func(ctx context.Context, path string) (BrowserResponse, error) {
		if path == browserRequest(grokWebRequests, grokAccountRequest) {
			return accountReply("0123456789ab"), nil
		}
		return fake.fetch(ctx, path)
	}
	stats, err := syncGrokWeb(t.Context(), d, fetch, BrowserSyncOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	assert.Equal(t, "grok-web:0123456789ab", sessionAccount(t, d, "grok-web:"+grokTestChat))
}

func TestSyncClaudeAIRecordsOrganization(t *testing.T) {
	const org = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name  string
		after string
		want  string
	}{
		{name: "same organizations at both ends", after: org, want: "claude-ai:" + org},
		{name: "organizations changed during the round", after: "33333333-3333-4333-8333-333333333333"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			orgRequests := 0
			detail := syncOneFetch(t, syncSummary, func() (BrowserResponse, error) {
				return BrowserResponse{Status: 200, Body: []byte(syncDetail)}, nil
			})
			fetch := func(ctx context.Context, path string) (BrowserResponse, error) {
				if path == claudeAIRequest(claudeAIOrganizationsRequest, "", "", 0) {
					orgRequests++
					uuid := org
					if orgRequests > 1 {
						uuid = tc.after
					}
					return BrowserResponse{Status: 200, Body: []byte(`[{"uuid":"` + uuid + `","capabilities":["chat"]}]`)}, nil
				}
				return detail(ctx, path)
			}
			stats, err := syncClaudeAI(t.Context(), d, fetch, BrowserSyncOptions{}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Imported)
			assert.Equal(t, 2, orgRequests, "the organizations are read at both ends of the round")
			assert.Equal(t, tc.want, sessionAccount(t, d, "claude-ai:22222222-2222-4222-8222-222222222222"))
		})
	}
}

// geminiAccounts fakes several signed-in Gemini accounts. slots[N] is the
// identity reply for /u/N; each slot lists one chat named after its slot.
type geminiAccounts struct {
	t     *testing.T
	slots []BrowserResponse
	asked []int
}

func geminiSlotChat(slot int) string { return "c_00000000000000a" + strconv.Itoa(slot) }

func (g *geminiAccounts) fetch(_ context.Context, path string) (BrowserResponse, error) {
	match := geminiSlotPattern.FindStringSubmatch(path)
	require.NotNil(g.t, match, "every Gemini request names its account slot: %s", path)
	slot, _ := strconv.Atoi(match[1])
	path = match[2]
	if path == geminiRequest(geminiAccountRequest, "", "") {
		g.asked = append(g.asked, slot)
		if slot >= len(g.slots) {
			return BrowserResponse{Status: 401}, nil
		}
		return g.slots[slot], nil
	}
	require.Less(g.t, slot, len(g.slots), "no requests past the last account: %s", path)
	chat := geminiSlotChat(slot)
	switch path {
	case geminiRequest(geminiListRequest, "", ""):
		entry := []any{chat, "Chat " + match[1], nil, nil, nil, []any{int64(1700000000), 0}}
		return BrowserResponse{Status: 200, Body: geminiRPCBody(g.t, geminiListRPC, []any{nil, nil, []any{entry}})}, nil
	case geminiRequest(geminiDetailRequest, chat, ""):
		turns := []any{geminiSyncTurn("r"+match[1], "Question "+match[1], "Answer", 1700000000)}
		return BrowserResponse{Status: 200, Body: geminiRPCBody(g.t, geminiDetailRPC, []any{turns, nil, nil, []any{}})}, nil
	}
	require.Failf(g.t, "unexpected Gemini request path", "%s", path)
	return BrowserResponse{}, nil
}

func TestSyncGeminiWalksSignedInAccounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots []BrowserResponse
		want  []string // account label of each synced slot's chat
		asked []int
	}{
		{
			name:  "stops at the first slot without a token",
			slots: []BrowserResponse{accountReply("00000000000a"), accountReply("00000000000b")},
			want:  []string{"gemini-apps:00000000000a", "gemini-apps:00000000000b"},
			asked: []int{0, 0, 1, 1, 2},
		},
		{
			name:  "stops at a repeated identity",
			slots: []BrowserResponse{accountReply("00000000000a"), accountReply("00000000000a")},
			want:  []string{"gemini-apps:00000000000a"},
			asked: []int{0, 0, 1},
		},
		{
			name:  "stops at an unrecognized identity after the first slot",
			slots: []BrowserResponse{accountReply("00000000000a"), accountReply("")},
			want:  []string{"gemini-apps:00000000000a"},
			asked: []int{0, 0, 1},
		},
		{
			name:  "first slot syncs unlabelled without an identity",
			slots: []BrowserResponse{accountReply("")},
			want:  []string{""},
			asked: []int{0, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			fake := &geminiAccounts{t: t, slots: tc.slots}
			stats, err := syncGeminiApps(t.Context(), d, fake.fetch, BrowserSyncOptions{}, nil)
			require.NoError(t, err)
			assert.Equal(t, len(tc.want), stats.Imported)
			for slot, want := range tc.want {
				assert.Equal(t, want, sessionAccount(t, d, "gemini-apps:"+geminiSlotChat(slot)), "slot %d", slot)
			}
			assert.Equal(t, tc.asked, fake.asked)
		})
	}
}

func TestSyncGeminiAccountLimit(t *testing.T) {
	d := testDB(t)
	slots := make([]BrowserResponse, 12)
	for i := range slots {
		slots[i] = accountReply(fmt.Sprintf("%012x", i+1))
	}
	fake := &geminiAccounts{t: t, slots: slots}
	stats, err := syncGeminiApps(t.Context(), d, fake.fetch, BrowserSyncOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 10, stats.Imported, "at most ten account slots are read")
}

func TestSyncGeminiAccountSlotsSwapped(t *testing.T) {
	d := testDB(t)
	fake := &geminiAccounts{t: t, slots: []BrowserResponse{accountReply("00000000000a"), accountReply("00000000000b")}}
	_, err := syncGeminiApps(t.Context(), d, fake.fetch, BrowserSyncOptions{}, nil)
	require.NoError(t, err)

	// The browser now lists the accounts in the other order, so slot 0's
	// chat is reported by account b. A set label is kept.
	fake.slots[0], fake.slots[1] = fake.slots[1], fake.slots[0]
	_, err = syncGeminiApps(t.Context(), d, fake.fetch, BrowserSyncOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "gemini-apps:00000000000a", sessionAccount(t, d, "gemini-apps:"+geminiSlotChat(0)))
	assert.Equal(t, "gemini-apps:00000000000b", sessionAccount(t, d, "gemini-apps:"+geminiSlotChat(1)))
}
