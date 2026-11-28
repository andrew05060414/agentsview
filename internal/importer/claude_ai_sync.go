package importer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

var claudeAIRequests = browserRequests(parser.AgentClaudeAI)

const (
	claudeAIOrganizationsRequest = iota
	claudeAIConversationsRequest
	claudeAIConversationRequest
)

func claudeAIRequest(shape int, organization, conversation string, offset int) string {
	return browserRequest(claudeAIRequests, shape, "{organization}", url.PathEscape(organization), "{conversation}", url.PathEscape(conversation), "{offset}", strconv.Itoa(offset))
}

func claudeAIPlatform() BrowserPlatform {
	return BrowserPlatform{
		Agent: parser.AgentClaudeAI, Label: "Claude.ai", ClientRelay: true,
		AuthRequired: claudeAIAuthRequired, Sync: syncClaudeAI,
	}
}

func claudeAIAuthRequired(status int, body []byte) bool {
	var apiError struct {
		Error struct {
			Details struct {
				Code string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &apiError)
	return status == 401 || apiError.Error.Details.Code == "account_session_invalid"
}

// claudeAICountsMarker refreshes usage-only fingerprints that cached a shorter
// branch with older counts. Full archives already stored the selected branch.
func claudeAICountsMarker(store db.Store) string {
	if storeArchiveContent(store).UsageOnly() {
		return "selected-branch-counts\x00"
	}
	return ""
}

func claudeAIFreshness(store db.Store, updatedAt, leaf string, session *db.Session) uint64 {
	return browserFreshness(claudeAICountsMarker(store)+updatedAt+"\x00"+leaf, session)
}

// syncClaudeAI imports changed conversations through the existing export
// importer. A non-empty opts.ClaudeAIOrganizations lists the lowercase
// organization UUIDs to import.
func syncClaudeAI(ctx context.Context, store BrowserSyncStore, fetch BrowserFetch, opts BrowserSyncOptions, cb *ImportCallbacks) (stats ImportStats, retErr error) {
	platform := claudeAIPlatform()
	orgs := opts.ClaudeAIOrganizations
	organizations, err := claudeAIChatOrganizations(ctx, platform, fetch)
	if err != nil {
		return stats, err
	}
	accountIDs := make(map[string][]string)

	runner := newBrowserSync(store, platform, &stats, opts, cb)
	for _, org := range organizations {
		if len(orgs) > 0 && !slices.Contains(orgs, strings.ToLower(org)) {
			continue
		}
		for offset := 0; ; {
			raw, err := fetchBrowser(ctx, platform, fetch, claudeAIRequest(claudeAIConversationsRequest, org, "", offset))
			if err != nil {
				return stats, err
			}
			var page struct {
				Data    []jsontext.Value `json:"data"`
				HasMore *bool            `json:"has_more"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return stats, err
			}
			items := page.Data
			if items == nil {
				return stats, errors.New("claude list had no data")
			}
			if len(items) == 0 {
				if page.HasMore != nil && *page.HasMore {
					return stats, errors.New("claude list returned an empty page with has_more: true")
				}
				break
			}
			for i, summary := range items {
				if err := ctx.Err(); err != nil {
					return stats, err
				}
				var marker struct {
					UUID        string         `json:"uuid"`
					UpdatedAt   string         `json:"updated_at"`
					CurrentLeaf jsontext.Value `json:"current_leaf_message_uuid"`
				}
				if err := json.Unmarshal(summary, &marker); err != nil || marker.UUID == "" || marker.UpdatedAt == "" {
					log.Printf("import: claude.ai list item %d of org %s skipped: uuid %q: uuid or updated_at is missing or not a string", offset+i, org, marker.UUID)
					stats.Errors++
					cb.progress(stats)
					continue
				}
				if len(marker.CurrentLeaf) == 0 {
					// Some list items carry no leaf field at all; they have no
					// message tree to import, so treat them like a null leaf.
					log.Printf("import: claude.ai conversation %s has no current_leaf_message_uuid field; skipping it", marker.UUID)
					marker.CurrentLeaf = jsontext.Value("null")
				}
				var leaf string
				if err := json.Unmarshal(marker.CurrentLeaf, &leaf); err != nil || leaf == "" && string(marker.CurrentLeaf) != "null" {
					log.Printf("import: claude.ai conversation %s skipped: current_leaf_message_uuid is %s, not a message uuid or null", marker.UUID, claudeAILeafKind(marker.CurrentLeaf))
					stats.Errors++
					cb.progress(stats)
					continue
				}
				if leaf == "" || leaf == "00000000-0000-4000-8000-000000000000" {
					runner.failedLast = false
					stats.Skipped++
					cb.progress(stats)
					continue
				}
				id := "claude-ai:" + marker.UUID
				accountIDs[org] = append(accountIDs[org], id)
				err := runner.conversation(ctx, id, browserConversation{
					freshness: func(session *db.Session) uint64 { return claudeAIFreshness(store, marker.UpdatedAt, leaf, session) },
					fetch: func(ctx context.Context) ([]byte, error) {
						return fetchBrowser(ctx, platform, fetch, claudeAIRequest(claudeAIConversationRequest, org, marker.UUID, 0))
					},
					parse: func(detail []byte) (parser.ParseResult, error) {
						result, err := parser.ParseClaudeAIDetail(detail)
						if err == nil && result.Session.ID != id {
							err = fmt.Errorf("conversation uuid differs from requested %s", marker.UUID)
						}
						return result, err
					},
					write: syncReplacing,
					matches: func(detail []byte, _ parser.ParseResult) bool {
						var current struct {
							UpdatedAt string `json:"updated_at"`
							Leaf      string `json:"current_leaf_message_uuid"`
						}
						return json.Unmarshal(detail, &current) == nil && current.UpdatedAt == marker.UpdatedAt && current.Leaf == leaf
					},
				})
				if err != nil {
					return stats, err
				}
			}
			offset += len(items)
			if page.HasMore != nil && !*page.HasMore {
				break
			}
		}
	}
	after, err := claudeAIChatOrganizations(ctx, platform, fetch)
	if err == nil && slices.Equal(organizations, after) {
		for _, org := range organizations {
			if err := runner.labelAccount(ctx, "claude-ai:"+org, accountIDs[org]); err != nil {
				return stats, err
			}
		}
	}
	return stats, ctx.Err()
}

func claudeAIChatOrganizations(ctx context.Context, platform BrowserPlatform, fetch BrowserFetch) ([]string, error) {
	raw, err := fetchBrowser(ctx, platform, fetch, claudeAIRequest(claudeAIOrganizationsRequest, "", "", 0))
	if err != nil {
		return nil, err
	}
	var organizations []struct {
		UUID         string   `json:"uuid"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &organizations); err != nil {
		return nil, err
	}
	if organizations == nil {
		return nil, errors.New("claude organizations must be an array")
	}
	ids := make([]string, 0, len(organizations))
	for _, org := range organizations {
		if org.UUID != "" && slices.Contains(org.Capabilities, "chat") {
			ids = append(ids, org.UUID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// claudeAILeafKind names the JSON shape of a list item's current leaf without
// copying its value into the log.
func claudeAILeafKind(raw jsontext.Value) string {
	if len(raw) == 0 {
		return "absent"
	}
	switch raw.Kind() {
	case '"':
		return "an empty string"
	case '0':
		return "a number"
	case 't', 'f':
		return "a boolean"
	case '{':
		return "an object"
	case '[':
		return "an array"
	}
	return "unreadable"
}
