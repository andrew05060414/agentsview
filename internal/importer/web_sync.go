package importer

import (
	"context"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

//go:embed gemini_requests.txt
var geminiRequests string

//go:embed grok_requests.txt
var grokRequests string

var (
	ErrGeminiAuthRequired  = errors.New("Gemini sign-in required")
	ErrGrokAuthRequired    = errors.New("Grok sign-in required")
	ErrWebResponseTooLarge = errors.New("browser response exceeds 32 MiB")
	errWebFormat           = errors.New("browser history is incomplete or its format changed; saved conversations were kept")
	geminiChatID           = regexp.MustCompile(`^c_[a-f0-9]{8,64}$`)
	webUUID                = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
)

var webSyncGap = 3 * time.Second

type webChat struct {
	id, title, updated string
}

type webPage struct {
	chats []webChat
	next  string
}

type webSyncConfig struct {
	platform chromehost.Platform
	agent    parser.AgentType
	auth     error
	shapes   string
	pinned   []int
	list     func([]byte) (webPage, error)
	detail   func(context.Context, func(context.Context, string) (ClaudeAIResponse, error), webChat) (parser.ParseResult, error)
}

// SyncGemini imports Gemini web history; Takeout activity identities stay separate.
func SyncGemini(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string) (ImportStats, error) {
	return syncWeb(ctx, store, fetch, cb, webSyncConfig{
		platform: chromehost.Gemini, agent: parser.AgentGeminiApps, auth: ErrGeminiAuthRequired,
		shapes: geminiRequests, pinned: []int{1, 0}, list: geminiList, detail: geminiDetail,
	}, machine...)
}

// SyncGrok imports grok.com history without touching local Grok agent sessions.
func SyncGrok(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, machine ...string) (ImportStats, error) {
	return syncWeb(ctx, store, fetch, cb, webSyncConfig{
		platform: chromehost.Grok, agent: parser.AgentGrok, auth: ErrGrokAuthRequired,
		shapes: grokRequests, pinned: []int{0}, list: grokList, detail: grokDetail,
	}, machine...)
}

func webRequest(shapes string, kind, pinned int, cursor, id string) string {
	return browserRequest(shapes, kind, "{pinned}", strconv.Itoa(pinned), "{cursor}", url.QueryEscape(cursor), "{geminichat}", id, "{conversation}", id)
}

func syncWeb(ctx context.Context, store BrowserSyncStore, fetch func(context.Context, string) (ClaudeAIResponse, error), cb *ImportCallbacks, config webSyncConfig, machine ...string) (stats ImportStats, retErr error) {
	unpaced := fetch
	fetch = func(ctx context.Context, path string) (ClaudeAIResponse, error) {
		if err := waitBrowserGap(ctx, webSyncGap); err != nil {
			return ClaudeAIResponse{}, err
		}
		return unpaced(ctx, path)
	}
	runner := browserSync{store: store, agent: config.agent, stats: &stats, cb: cb, machine: machine,
		tooLarge: ErrWebResponseTooLarge, stop: func(err error) bool {
			var httpErr *browserHTTPError
			return errors.Is(err, config.auth) || errors.As(err, &httpErr) && (httpErr.status == 403 || httpErr.rateLimited)
		}}
	seen := make(map[string]bool)
	for _, pinned := range config.pinned {
		cursor := ""
		cursors := make(map[string]bool)
		for {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if cursors[cursor] {
				return stats, errors.New("browser history repeated a page cursor")
			}
			cursors[cursor] = true
			raw, err := fetchBrowser(ctx, fetch, config.platform, webRequest(config.shapes, 0, pinned, cursor, ""))
			if err != nil {
				return stats, err
			}
			page, err := config.list(raw)
			if err != nil {
				return stats, err
			}
			if len(page.chats) == 0 && page.next != "" {
				return stats, errWebFormat
			}
			for _, marker := range page.chats {
				id := string(config.agent) + ":web:" + marker.id
				if seen[id] {
					continue
				}
				seen[id] = true
				var parsed parser.ParseResult
				err := runner.conversation(ctx, id, browserConversation{
					freshness: func(session *db.Session) uint64 { return browserFreshness(marker.updated+"\x00"+marker.title, session) },
					fetch: func(ctx context.Context) ([]byte, error) {
						var err error
						parsed, err = config.detail(ctx, fetch, marker)
						return nil, err
					},
					parse: func([]byte) (parser.ParseResult, error) { return parsed, nil },
					write: func(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
						// Native timestamps are absent from some Gemini turns. Preserve an existing start.
						if result.Session.StartedAt.IsZero() {
							old, err := store.GetSessionFull(ctx, id)
							if err != nil {
								return importSkipped, err
							}
							if old != nil && old.StartedAt != nil {
								result.Session.StartedAt, _ = db.ParseStoredTimestamp(*old.StartedAt)
							}
						}
						return replaceWebConversation(ctx, store, result)
					},
					matches: func([]byte, parser.ParseResult) bool { return false },
				})
				if err != nil {
					return stats, err
				}
			}
			if page.next == "" {
				break
			}
			cursor = page.next
		}
	}
	return stats, nil
}

// Web endpoints lack a verified list/detail revision pair. Re-read detail on
// each run rather than caching an unverified freshness marker. Identical bodies
// still skip writes. A changed body always retains the previous version.
func replaceWebConversation(ctx context.Context, store db.Store, result parser.ParseResult) (importStatus, error) {
	old, err := store.GetSessionFull(ctx, result.Session.ID)
	if err != nil {
		return importSkipped, err
	}
	if old == nil {
		return syncConversation(ctx, store, result)
	}
	if old.Agent != string(result.Session.Agent) {
		return importSkipped, errors.New("existing browser session belongs to another agent")
	}
	archived, err := store.GetAllMessages(ctx, result.Session.ID)
	if err != nil {
		return importSkipped, err
	}
	msgs := claudeAIMessages(result.Session.ID, result.Messages)
	if old.MessageCount == result.Session.MessageCount && sameMessages(archived, storedFormMessages(store, msgs)) {
		return upsertConversation(ctx, store, result, nil)
	}
	return replaceSyncedConversation(ctx, store, result, msgs, true)
}

func geminiRPC(raw []byte, rpc string) ([]any, error) {
	// batchexecute uses an XSSI prefix and newline-delimited length frames.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		var rows [][]any
		if json.Unmarshal([]byte(line), &rows) != nil {
			return nil, errWebFormat
		}
		for _, row := range rows {
			if textAt(row, 0) != "wrb.fr" || textAt(row, 1) != rpc {
				continue
			}
			var payload []any
			if json.Unmarshal([]byte(textAt(row, 2)), &payload) != nil {
				return nil, errWebFormat
			}
			return payload, nil
		}
	}
	return nil, errWebFormat
}

func at(value any, indexes ...int) any {
	for _, index := range indexes {
		array, ok := value.([]any)
		if !ok || index < 0 || index >= len(array) {
			return nil
		}
		value = array[index]
	}
	return value
}

func textAt(value any, indexes ...int) string { text, _ := at(value, indexes...).(string); return text }

func geminiList(raw []byte) (webPage, error) {
	body, err := geminiRPC(raw, "MaZiqc")
	if err != nil {
		return webPage{}, err
	}
	rows, ok := at(body, 2).([]any)
	if !ok {
		return webPage{}, errWebFormat
	}
	page := webPage{next: textAt(body, 1)}
	for _, row := range rows {
		id := textAt(row, 0)
		stamp, err := json.Marshal(at(row, 5))
		if !geminiChatID.MatchString(id) || err != nil {
			return webPage{}, errWebFormat
		}
		page.chats = append(page.chats, webChat{id: id, title: textAt(row, 1), updated: string(stamp)})
	}
	return page, nil
}

func geminiDetail(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), marker webChat) (parser.ParseResult, error) {
	for limit := 100; limit <= 12800; limit *= 2 {
		path := strings.ReplaceAll(webRequest(geminiRequests, 1, 0, "", marker.id), "{limit}", strconv.Itoa(limit))
		raw, err := fetchBrowser(ctx, fetch, chromehost.Gemini, path)
		if err != nil {
			return parser.ParseResult{}, err
		}
		body, err := geminiRPC(raw, "hNvQHb")
		if err != nil {
			return parser.ParseResult{}, err
		}
		turns, ok := at(body, 0).([]any)
		if !ok || len(turns) == 0 {
			return parser.ParseResult{}, errWebFormat
		}
		if len(turns) >= limit || textAt(body, 1) != "" {
			continue
		}
		result := newWebResult(parser.AgentGeminiApps, marker)
		for _, turn := range slices.Backward(turns) {
			if textAt(turn, 0, 0) != marker.id {
				return parser.ParseResult{}, errWebFormat
			}
			question := textAt(turn, 2, 0, 0)
			answer := textAt(turn, 3, 0, 0, 1, 0)
			status, _ := at(turn, 3, 0, 0, 8, 0).(float64)
			if question == "" || answer == "" || status != 2 {
				return parser.ParseResult{}, errWebFormat
			}
			rid := textAt(turn, 0, 1)
			appendWebMessage(&result, parser.RoleUser, question, rid+":user", "", time.Time{})
			appendWebMessage(&result, parser.RoleAssistant, answer, textAt(turn, 3, 0, 0, 0), "", time.Time{})
		}
		return result, nil
	}
	return parser.ParseResult{}, errors.New("Gemini history exceeds the supported complete-read limit; saved conversation was kept")
}

func grokList(raw []byte) (webPage, error) {
	var body struct {
		Conversations []struct {
			ConversationID string `json:"conversationId"`
			Title          string `json:"title"`
			ModifyTime     string `json:"modifyTime"`
		} `json:"conversations"`
		NextPageToken string `json:"nextPageToken"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Conversations == nil {
		return webPage{}, errWebFormat
	}
	page := webPage{next: body.NextPageToken}
	for _, row := range body.Conversations {
		if !webUUID.MatchString(row.ConversationID) {
			return webPage{}, errWebFormat
		}
		page.chats = append(page.chats, webChat{id: row.ConversationID, title: row.Title, updated: row.ModifyTime})
	}
	return page, nil
}

func grokDetail(ctx context.Context, fetch func(context.Context, string) (ClaudeAIResponse, error), marker webChat) (parser.ParseResult, error) {
	raw, err := fetchBrowser(ctx, fetch, chromehost.Grok, webRequest(grokRequests, 1, 0, "", marker.id))
	if err != nil {
		return parser.ParseResult{}, err
	}
	var body struct {
		ResponseNodes []struct {
			ResponseID string `json:"responseId"`
		} `json:"responseNodes"`
		InflightResponses []any `json:"inflightResponses"`
		Responses         []struct {
			ResponseID       string `json:"responseId"`
			ParentResponseID string `json:"parentResponseId"`
			Sender           string `json:"sender"`
			Message          string `json:"message"`
			CreateTime       string `json:"createTime"`
		} `json:"responses"`
	}
	if json.Unmarshal(raw, &body) != nil || body.ResponseNodes == nil || body.Responses == nil || len(body.InflightResponses) > 0 {
		return parser.ParseResult{}, errWebFormat
	}
	// Until a producer-defined active-leaf marker is verified, refuse branches.
	if len(body.Responses) == 0 || len(body.Responses) != len(body.ResponseNodes) {
		return parser.ParseResult{}, errWebFormat
	}
	byID := make(map[string]int)
	parents := make(map[string]bool)
	for i, message := range body.Responses {
		if !webUUID.MatchString(message.ResponseID) || message.Message == "" {
			return parser.ParseResult{}, errWebFormat
		}
		if _, duplicate := byID[message.ResponseID]; duplicate {
			return parser.ParseResult{}, errWebFormat
		}
		byID[message.ResponseID] = i
		if message.ParentResponseID != "" {
			parents[message.ParentResponseID] = true
		}
	}
	for _, node := range body.ResponseNodes {
		if _, ok := byID[node.ResponseID]; !ok {
			return parser.ParseResult{}, errWebFormat
		}
	}
	leaf := ""
	for id := range byID {
		if !parents[id] {
			if leaf != "" {
				return parser.ParseResult{}, errWebFormat
			}
			leaf = id
		}
	}
	var chain []int
	visited := make(map[string]bool)
	for leaf != "" {
		index, ok := byID[leaf]
		if !ok || visited[leaf] {
			return parser.ParseResult{}, errWebFormat
		}
		visited[leaf] = true
		chain = append(chain, index)
		leaf = body.Responses[index].ParentResponseID
	}
	if len(chain) != len(body.Responses) {
		return parser.ParseResult{}, errWebFormat
	}
	result := newWebResult(parser.AgentGrok, marker)
	for _, index := range slices.Backward(chain) {
		message := body.Responses[index]
		role := parser.RoleAssistant
		switch message.Sender {
		case "human":
			role = parser.RoleUser
		case "assistant":
		default:
			return parser.ParseResult{}, errWebFormat
		}
		stamp, err := time.Parse(time.RFC3339Nano, message.CreateTime)
		if err != nil {
			return parser.ParseResult{}, errWebFormat
		}
		appendWebMessage(&result, role, message.Message, message.ResponseID, message.ParentResponseID, stamp)
	}
	return result, nil
}

func newWebResult(agent parser.AgentType, marker webChat) parser.ParseResult {
	return parser.ParseResult{Session: parser.ParsedSession{ID: string(agent) + ":web:" + marker.id,
		Agent: agent, Project: string(agent), SessionName: marker.title, SessionNamePresent: true}}
}

func appendWebMessage(result *parser.ParseResult, role parser.RoleType, content, source, parent string, timestamp time.Time) {
	result.Messages = append(result.Messages, parser.ParsedMessage{Ordinal: len(result.Messages), Role: role,
		Content: content, ContentLength: len(content), SourceUUID: source, SourceParentUUID: parent, Timestamp: timestamp})
	result.Session.MessageCount++
	if role == parser.RoleUser {
		result.Session.UserMessageCount++
		if result.Session.FirstMessage == "" {
			result.Session.FirstMessage = content
		}
	}
	if !timestamp.IsZero() {
		if result.Session.StartedAt.IsZero() || timestamp.Before(result.Session.StartedAt) {
			result.Session.StartedAt = timestamp
		}
		if timestamp.After(result.Session.EndedAt) {
			result.Session.EndedAt = timestamp
		}
	}
}
