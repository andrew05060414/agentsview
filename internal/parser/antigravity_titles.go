package parser

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AntigravitySummariesDBName is the per-root store where Antigravity keeps
// conversation titles and workspace metadata, apart from each conversation's transcript.
const AntigravitySummariesDBName = "conversation_summaries.db"

const antigravityTitleQuery = "SELECT conversation_id, title FROM conversation_summaries"

// AntigravitySummary holds metadata extracted from conversation_summaries.db.
type AntigravitySummary struct {
	Title                string
	TitlePresent         bool
	Cwd                  string
	Project              string
	ParentConversationID string
}

type antigravitySummarySnapshot struct {
	state     SQLiteContainerState
	summaries map[string]AntigravitySummary
	err       error
	readAt    time.Time
}

var antigravitySummaryCache = struct {
	sync.Mutex
	stores map[string]antigravitySummarySnapshot
}{stores: make(map[string]antigravitySummarySnapshot)}

func InvalidateAntigravityTitles(path string) {
	antigravitySummaryCache.Lock()
	defer antigravitySummaryCache.Unlock()
	delete(antigravitySummaryCache.stores, path)
}

// AntigravityStoreSummaries returns the cached or freshly read conversation summaries.
// Matching markers can hide writes, so cached summaries expire.
func AntigravityStoreSummaries(ctx context.Context, path string) (map[string]AntigravitySummary, SQLiteContainerState, error) {
	state, ok := StatSQLiteContainerState(path)
	antigravitySummaryCache.Lock()
	defer antigravitySummaryCache.Unlock()
	now := sqliteTitleNow()
	if prior, cached := antigravitySummaryCache.stores[path]; ok && cached && prior.state == state && now.Sub(prior.readAt) < SQLiteContainerDigestVerifyInterval {
		return prior.summaries, prior.state, prior.err
	}
	summaries, err := readAntigravitySummaries(ctx, path)
	if ctx.Err() != nil {
		return nil, state, ctx.Err()
	}
	if ok {
		// Remote imports extract stores into fresh temp roots; bound what stays cached.
		if _, cached := antigravitySummaryCache.stores[path]; !cached && len(antigravitySummaryCache.stores) >= 16 {
			clear(antigravitySummaryCache.stores)
		}
		antigravitySummaryCache.stores[path] = antigravitySummarySnapshot{state: state, summaries: summaries, err: err, readAt: now}
	} else {
		delete(antigravitySummaryCache.stores, path)
	}
	return summaries, state, err
}

// Matching markers can hide writes, so cached titles expire.
func AntigravityStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "conversation_summaries", "conversation_id", antigravityTitleQuery)
}

func readAntigravitySummaries(ctx context.Context, path string) (map[string]AntigravitySummary, error) {
	if !IsRegularFile(path) {
		return nil, nil
	}
	observeSharedContainerScan(ctx)
	store, err := openSQLiteReadOnly(path, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, err
	}
	defer store.Close()

	// Inspect available columns so stores without workspace_uris or
	// parent_conversation_id still yield titles.
	rows, err := store.QueryContext(ctx, "PRAGMA table_info(conversation_summaries)")
	if err != nil {
		return nil, err
	}
	hasWorkspace := false
	hasParent := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if name == "workspace_uris" {
			hasWorkspace = true
		} else if name == "parent_conversation_id" {
			hasParent = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	query := "SELECT conversation_id, title"
	if hasWorkspace {
		query += ", workspace_uris"
	}
	if hasParent {
		query += ", parent_conversation_id"
	}
	query += " FROM conversation_summaries"

	queryRows, err := store.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer queryRows.Close()

	summaries := make(map[string]AntigravitySummary)
	for queryRows.Next() {
		var id string
		var title sql.NullString
		var workspaceURIs sql.NullString
		var parentID sql.NullString

		scanDests := []any{&id, &title}
		if hasWorkspace {
			scanDests = append(scanDests, &workspaceURIs)
		}
		if hasParent {
			scanDests = append(scanDests, &parentID)
		}
		if err := queryRows.Scan(scanDests...); err != nil {
			return nil, err
		}

		var summary AntigravitySummary
		if title.Valid {
			summary.TitlePresent = true
			summary.Title = strings.TrimSpace(title.String)
		}
		if hasWorkspace && workspaceURIs.Valid && workspaceURIs.String != "" {
			cwd := extractAntigravityWorkspace(workspaceURIs.String)
			summary.Cwd = cwd
			if cwd != "" {
				summary.Project = ExtractProjectFromCwdWithBranchContext(ctx, cwd, "")
			}
		}
		if hasParent && parentID.Valid {
			summary.ParentConversationID = strings.TrimSpace(parentID.String)
		}
		summaries[id] = summary
	}
	if err := queryRows.Err(); err != nil {
		return nil, err
	}
	return summaries, nil
}

// parseAntigravityWorkspaceURI decodes a file:// URI into a clean filesystem path.
// It handles percent-encoded characters, Windows drive letters with leading slashes,
// UNC network shares (file://server/share), and localhost authority.
func parseAntigravityWorkspaceURI(rawURI string) string {
	rawURI = strings.TrimSpace(rawURI)
	if rawURI == "" {
		return ""
	}
	if !strings.HasPrefix(rawURI, "file://") {
		return filepath.Clean(filepath.FromSlash(rawURI))
	}
	parsed, err := url.Parse(rawURI)
	if err != nil {
		unescaped, uerr := url.PathUnescape(rawURI)
		if uerr == nil {
			rawURI = unescaped
		}
		p := strings.TrimPrefix(rawURI, "file://")
		return filepath.Clean(filepath.FromSlash(p))
	}

	// Handle Windows UNC network share: file://server/share/repo -> \\server\share\repo
	if parsed.Host != "" && parsed.Host != "localhost" {
		unc := "//" + parsed.Host + parsed.Path
		return filepath.Clean(filepath.FromSlash(unc))
	}

	p := parsed.Path
	// On Windows, file:///D:/foo or file:///d%3A/foo produces /D:/foo or /d:/foo.
	if len(p) >= 3 && p[0] == '/' && (p[2] == ':' || p[2] == '|') &&
		((p[1] >= 'a' && p[1] <= 'z') || (p[1] >= 'A' && p[1] <= 'Z')) {
		drive := p[1]
		rest := p[3:]
		p = string(drive) + ":" + rest
	}
	return filepath.Clean(filepath.FromSlash(p))
}

func extractAntigravityWorkspace(workspaceURIsJSON string) string {
	workspaceURIsJSON = strings.TrimSpace(workspaceURIsJSON)
	if workspaceURIsJSON == "" || workspaceURIsJSON == "[]" {
		return ""
	}
	var uris []string
	if err := json.Unmarshal([]byte(workspaceURIsJSON), &uris); err != nil {
		var singleURI string
		if jsonErr := json.Unmarshal([]byte(workspaceURIsJSON), &singleURI); jsonErr == nil && singleURI != "" {
			return parseAntigravityWorkspaceURI(singleURI)
		}
		return parseAntigravityWorkspaceURI(workspaceURIsJSON)
	}
	for _, u := range uris {
		p := parseAntigravityWorkspaceURI(u)
		if p != "" {
			return p
		}
	}
	return ""
}

// AntigravitySessionRoot returns the Antigravity root that owns a stored
// conversation database or brain transcript path, even after it was deleted.
func AntigravitySessionRoot(path string) (string, bool) {
	path = filepath.Clean(path)
	if root, _, ok := antigravityBrainTranscriptConversation(path); ok {
		return root, true
	}
	root := filepath.Dir(filepath.Dir(path))
	if _, _, ok := antigravityConversationDBForPath(root, path); ok {
		return root, true
	}
	return "", false
}

// applyAntigravityTitle sets the session's title and workspace from its root's store.
func applyAntigravityTitle(ctx context.Context, sess *ParsedSession, src antigravitySource) {
	applyAntigravitySummary(ctx, sess, src)
}

// applyAntigravitySummary sets the session's title, cwd, project, and parent from its root's store.
// An unreadable store or missing entry leaves no title signal, so the stored
// name survives the write.
func applyAntigravitySummary(ctx context.Context, sess *ParsedSession, src antigravitySource) {
	summaries, _, err := AntigravityStoreSummaries(ctx, filepath.Join(src.Root, AntigravitySummariesDBName))
	if err != nil {
		return
	}
	summary, ok := summaries[src.ID]
	if !ok {
		return
	}
	if summary.TitlePresent {
		sess.SessionName = summary.Title
		sess.SessionNamePresent = true
	}
	if sess.Cwd == "" && summary.Cwd != "" {
		sess.Cwd = summary.Cwd
	}
	if sess.Project == "" {
		if summary.Project != "" {
			sess.Project = summary.Project
		} else if sess.Cwd != "" {
			sess.Project = ExtractProjectFromCwdWithBranchContext(ctx, sess.Cwd, "")
		}
	}
	if sess.ParentSessionID == "" && summary.ParentConversationID != "" {
		sess.ParentSessionID = antigravityIDPrefix + summary.ParentConversationID
		if sess.RelationshipType == "" {
			sess.RelationshipType = RelSubagent
		}
	}
}

// PreservesStoredSessionName reports whether a parse carries no title signal
// for an agent whose titles live outside the transcript.
func PreservesStoredSessionName(sess ParsedSession) bool {
	return (sess.Agent == AgentCodex ||
		sess.Agent == AgentAntigravity ||
		sess.Agent == AgentQoder) && !sess.SessionNamePresent
}
