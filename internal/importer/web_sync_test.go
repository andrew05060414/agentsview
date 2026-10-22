package importer

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/config"
)

func init() { webSyncGap = 0 }

const webTestChat = "22222222-2222-4222-8222-222222222222"
const webTestUser = "33333333-3333-4333-8333-333333333333"
const webTestAnswer = "44444444-4444-4444-8444-444444444444"

func rpcFixture(t *testing.T, rpc string, payload any) []byte {
	t.Helper()
	inner, err := json.Marshal(payload)
	require.NoError(t, err)
	raw, err := json.Marshal([]any{[]any{"wrb.fr", rpc, string(inner), nil}})
	require.NoError(t, err)
	return append([]byte(")]}'\n\n100\n"), raw...)
}

func geminiFixture(t *testing.T, answer string) []byte {
	t.Helper()
	candidate := []any{"rc_answer", []any{answer}, nil, nil, nil, nil, nil, nil, []any{2}}
	turn := []any{[]any{"c_12345678", "r_turn"}, nil, []any{[]any{"Question"}}, []any{[]any{candidate}}}
	return rpcFixture(t, "hNvQHb", []any{[]any{turn}})
}

func grokFixture(answer string) []byte {
	return []byte(`{"responseNodes":[{"responseId":"` + webTestUser + `"},{"responseId":"` + webTestAnswer + `"}],"inflightResponses":[],"responses":[` +
		`{"responseId":"` + webTestAnswer + `","parentResponseId":"` + webTestUser + `","sender":"assistant","message":"` + answer + `","createTime":"2026-01-01T00:01:00Z"},` +
		`{"responseId":"` + webTestUser + `","sender":"human","message":"Question","createTime":"2026-01-01T00:00:00Z"}]}`)
}

func webFixtureFetch(t *testing.T, platform string, detail *[]byte, details *int) func(context.Context, string) (ClaudeAIResponse, error) {
	t.Helper()
	return func(_ context.Context, path string) (ClaudeAIResponse, error) {
		var body []byte
		if platform == "gemini" {
			switch path {
			case "/agentsview-sync/conversations?pinned=1&cursor=", "/agentsview-sync/conversations?pinned=0&cursor=":
				body = rpcFixture(t, "MaZiqc", []any{nil, nil, []any{[]any{"c_12345678", "Title", false, nil, nil, []any{100, 0}}}})
			case "/agentsview-sync/conversation/c_12345678?limit=100":
				*details++
				body = *detail
			default:
				require.Failf(t, "unexpected read path", "%s", path)
			}
		} else {
			switch path {
			case "/rest/app-chat/conversations?pageSize=50&pageToken=":
				body = []byte(`{"conversations":[{"conversationId":"` + webTestChat + `","title":"Title","modifyTime":"2026-01-01T00:01:00Z"}]}`)
			case "/rest/app-chat/conversations/" + webTestChat + "/response-node?includeThreads=true":
				*details++
				body = *detail
			default:
				require.Failf(t, "unexpected read path", "%s", path)
			}
		}
		return ClaudeAIResponse{Status: 200, Body: body}, nil
	}
}

func TestWebSyncPreservesEditsAndRetriesIncompleteHistory(t *testing.T) {
	for _, platform := range []string{"gemini", "grok"} {
		t.Run(platform, func(t *testing.T) {
			d := testDB(t)
			sync := SyncGemini
			id := "gemini-apps:web:c_12345678"
			detail := geminiFixture(t, "Answer")
			changed := geminiFixture(t, "Edited answer")
			if platform == "grok" {
				sync, id, detail, changed = SyncGrok, "grok:web:"+webTestChat, grokFixture("Answer"), grokFixture("Edited answer")
			}
			details := 0
			fetch := webFixtureFetch(t, platform, &detail, &details)
			stats, err := sync(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Imported)
			assert.Zero(t, stats.Errors)
			stats, err = sync(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 2, details, "unverified list versions must never suppress a detail read")
			detail = changed
			stats, err = sync(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Updated)
			assert.Len(t, replacedCopies(t, d, id), 1)
			messages, err := d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Edited answer"}, messageContents(messages))
			copies := replacedCopies(t, d, id)
			require.Len(t, copies, 1)
			previous, err := d.GetAllMessages(t.Context(), copies[0].ID)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Answer"}, messageContents(previous))
			detail = []byte(`{"unexpected":true}`)
			stats, err = sync(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Errors)
			messages, err = d.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, []string{"Question", "Edited answer"}, messageContents(messages))
			detail = changed
			stats, err = sync(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Equal(t, 5, details)
		})
	}
}

func TestWebSyncListFailuresAndCancellation(t *testing.T) {
	for _, sync := range []func(context.Context, BrowserSyncStore, func(context.Context, string) (ClaudeAIResponse, error), *ImportCallbacks, ...string) (ImportStats, error){SyncGemini, SyncGrok} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := sync(ctx, testDB(t), func(context.Context, string) (ClaudeAIResponse, error) {
			require.Fail(t, "canceled sync must not fetch")
			return ClaudeAIResponse{}, nil
		}, nil)
		require.ErrorIs(t, err, context.Canceled)
		_, err = sync(t.Context(), testDB(t), func(context.Context, string) (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(`{"unexpected":true}`)}, nil
		}, nil)
		require.ErrorIs(t, err, errWebFormat)
	}
}

func TestWebBrowserHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		platform chromehost.Platform
		auth     error
	}{{chromehost.Gemini, ErrGeminiAuthRequired}, {chromehost.Grok, ErrGrokAuthRequired}} {
		for _, status := range []int{401, 403, 413, 429, 500} {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				_, err := fetchBrowser(t.Context(), func(context.Context, string) (ClaudeAIResponse, error) {
					calls++
					retryAfter := ""
					if status == 429 {
						retryAfter = "120"
					}
					return ClaudeAIResponse{Status: status, RetryAfter: retryAfter}, nil
				}, tc.platform, "/read")
				require.Error(t, err)
				if status == 401 {
					assert.ErrorIs(t, err, tc.auth)
				}
				if status == 413 {
					assert.ErrorIs(t, err, ErrWebResponseTooLarge)
				}
				if status == 500 {
					assert.Equal(t, 5, calls)
				} else {
					assert.Equal(t, 1, calls)
				}
			})
		}
	}
}

func TestGrokRefusesPartialBranchCycleAndWrongRole(t *testing.T) {
	good := string(grokFixture("Answer"))
	for _, raw := range []string{
		strings.Replace(good, `"inflightResponses":[]`, `"inflightResponses":[{}]`, 1),
		strings.Replace(good, `"parentResponseId":"`+webTestUser+`",`, "", 1),
		strings.Replace(good, `"sender":"human"`, `"sender":"unknown"`, 1),
		strings.Replace(good, `"sender":"human"`, `"sender":"human","parentResponseId":"`+webTestAnswer+`"`, 1),
		strings.Replace(good, `"createTime":"2026-01-01T00:00:00Z"`, `"createTime":"bad"`, 1),
	} {
		_, err := grokDetail(t.Context(), func(context.Context, string) (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: []byte(raw)}, nil
		}, webChat{id: webTestChat})
		assert.ErrorIs(t, err, errWebFormat)
	}
}

func TestGeminiRejectsUnfinishedAndMismatchedHistory(t *testing.T) {
	for _, raw := range [][]byte{[]byte("unrecognized framing"), geminiFixture(t, ""), []byte(strings.ReplaceAll(string(geminiFixture(t, "Answer")), "c_12345678", "c_87654321"))} {
		_, err := geminiDetail(t.Context(), func(context.Context, string) (ClaudeAIResponse, error) {
			return ClaudeAIResponse{Status: 200, Body: raw}, nil
		}, webChat{id: "c_12345678"})
		assert.ErrorIs(t, err, errWebFormat)
	}
}

func TestWebSyncCursorCycleStops(t *testing.T) {
	d := testDB(t)
	detail := grokFixture("Answer")
	calls := 0
	_, err := SyncGrok(t.Context(), d, func(_ context.Context, path string) (ClaudeAIResponse, error) {
		if strings.Contains(path, "response-node") {
			return ClaudeAIResponse{Status: 200, Body: detail}, nil
		}
		calls++
		return ClaudeAIResponse{Status: 200, Body: []byte(`{"conversations":[{"conversationId":"` + webTestChat + `"}],"nextPageToken":"same"}`)}, nil
	}, nil)
	require.ErrorContains(t, err, "repeated a page cursor")
	assert.Equal(t, 2, calls)
}

func TestWebSyncRespectsTrashExclusionAndProgressCancellation(t *testing.T) {
	for _, platform := range []string{"gemini", "grok"} {
		for _, excluded := range []bool{false, true} {
			d := testDB(t)
			sync, id, detail := SyncGemini, "gemini-apps:web:c_12345678", geminiFixture(t, "Answer")
			if platform == "grok" {
				sync, id, detail = SyncGrok, "grok:web:"+webTestChat, grokFixture("Answer")
			}
			details := 0
			fetch := webFixtureFetch(t, platform, &detail, &details)
			ctx, cancel := context.WithCancel(t.Context())
			stats, err := sync(ctx, d, fetch, &ImportCallbacks{OnProgress: func(ImportStats) { cancel() }})
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, 1, stats.Imported, "committed work remains counted after cancellation")
			require.NoError(t, d.SoftDeleteSession(t.Context(), id))
			if excluded {
				_, err := d.DeleteSessionIfTrashed(t.Context(), id)
				require.NoError(t, err)
			}
			stats, err = sync(t.Context(), d, fetch, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, stats.Skipped)
			assert.Zero(t, stats.Errors)
			assert.Equal(t, 1, details, "trash and exclusion skip detail fetches")
		}
	}
}

func TestWebSyncUsageArchiveKeepsCountsOnShorterHistory(t *testing.T) {
	d := testDB(t)
	d.SetArchiveContent(config.ArchiveContentUsage)
	detail := grokFixture("Answer")
	details := 0
	fetch := webFixtureFetch(t, "grok", &detail, &details)
	stats, err := SyncGrok(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Imported)
	var body map[string]any
	require.NoError(t, json.Unmarshal(detail, &body))
	body["responses"] = body["responses"].([]any)[1:]
	body["responseNodes"] = body["responseNodes"].([]any)[:1]
	detail, err = json.Marshal(body)
	require.NoError(t, err)
	stats, err = SyncGrok(t.Context(), d, fetch, nil)
	require.NoError(t, err)
	assert.Zero(t, stats.Errors)
	assert.Equal(t, 1, stats.Updated)
	session, err := d.GetSessionFull(t.Context(), "grok:web:"+webTestChat)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 1, session.MessageCount)
	copies := replacedCopies(t, d, session.ID)
	require.Len(t, copies, 1)
	assert.Equal(t, 2, copies[0].MessageCount)
}
