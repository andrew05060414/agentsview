package parser

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AntigravitySummariesDBName is the per-root store where Antigravity keeps
// conversation titles and workspace metadata, apart from each conversation's
// transcript.
const AntigravitySummariesDBName = "conversation_summaries.db"

const antigravityTitleQuery = "SELECT conversation_id, title FROM conversation_summaries"

// Matching markers can hide writes, so cached titles expire.
func AntigravityStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "conversation_summaries", "conversation_id", antigravityTitleQuery)
}

// AntigravitySummary holds the workspace metadata Antigravity keeps in
// conversation_summaries.db next to the title. Titles are read separately
// through AntigravityStoreTitles.
type AntigravitySummary struct {
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

var antigravitySummaryNow = time.Now

var antigravitySummaryCache = struct {
	sync.Mutex
	stores map[string]antigravitySummarySnapshot
}{stores: make(map[string]antigravitySummarySnapshot)}

// InvalidateAntigravitySummaries drops the cached workspace metadata read.
func InvalidateAntigravitySummaries(path string) {
	antigravitySummaryCache.Lock()
	defer antigravitySummaryCache.Unlock()
	delete(antigravitySummaryCache.stores, path)
}

// AntigravityStoreSummaries returns the cached or freshly read workspace
// metadata (cwd, project, parent conversation) keyed by conversation ID.
// Matching markers can hide writes, so cached reads expire.
func AntigravityStoreSummaries(ctx context.Context, path string) (map[string]AntigravitySummary, SQLiteContainerState, error) {
	state, ok := StatSQLiteContainerState(path)
	antigravitySummaryCache.Lock()
	defer antigravitySummaryCache.Unlock()
	now := antigravitySummaryNow()
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

	// Older schemas lack workspace_uris or parent_conversation_id.
	var hasID, hasWorkspace, hasParent bool
	if err := store.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'conversation_id'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'workspace_uris'),
		EXISTS(SELECT 1 FROM pragma_table_info('conversation_summaries') WHERE name = 'parent_conversation_id')`,
	).Scan(&hasID, &hasWorkspace, &hasParent); err != nil {
		return nil, err
	}
	if !hasID || (!hasWorkspace && !hasParent) {
		return nil, nil
	}
	workspaceExpr, parentExpr := "NULL", "NULL"
	if hasWorkspace {
		workspaceExpr = "workspace_uris"
	}
	if hasParent {
		parentExpr = "parent_conversation_id"
	}
	rows, err := store.QueryContext(ctx,
		"SELECT conversation_id, "+workspaceExpr+", "+parentExpr+" FROM conversation_summaries")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summaries := make(map[string]AntigravitySummary)
	for rows.Next() {
		var id string
		var workspaceURIs, parentID sql.NullString
		if err := rows.Scan(&id, &workspaceURIs, &parentID); err != nil {
			return nil, err
		}
		var summary AntigravitySummary
		if workspaceURIs.Valid && workspaceURIs.String != "" {
			summary.Cwd = extractAntigravityWorkspace(workspaceURIs.String)
			if summary.Cwd != "" {
				summary.Project = ExtractProjectFromCwdWithBranchContext(ctx, summary.Cwd, "")
			}
		}
		if parentID.Valid {
			summary.ParentConversationID = strings.TrimSpace(parentID.String)
		}
		if summary != (AntigravitySummary{}) {
			summaries[id] = summary
		}
	}
	if err := rows.Err(); err != nil {
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
	// Also handle file:///D|/foo which is sometimes seen in browsers/URIs.
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

// applyAntigravityTitle sets the session's title from its root's store. An
// unreadable store or missing entry leaves no title signal, so the stored
// name survives the write. It also applies the store's workspace metadata.
func applyAntigravityTitle(ctx context.Context, sess *ParsedSession, src antigravitySource) {
	applyAntigravitySummary(ctx, sess, src)
	titles, _, err := AntigravityStoreTitles(ctx, filepath.Join(src.Root, AntigravitySummariesDBName))
	if err != nil {
		return
	}
	if title, ok := titles[src.ID]; ok {
		sess.SessionName = title
		sess.SessionNamePresent = true
	}
}

// applyAntigravitySummary fills the session's cwd, project, and parent from
// its root's store without overriding values the transcript already set.
func applyAntigravitySummary(ctx context.Context, sess *ParsedSession, src antigravitySource) {
	summaries, _, err := AntigravityStoreSummaries(ctx, filepath.Join(src.Root, AntigravitySummariesDBName))
	if err != nil {
		return
	}
	summary, ok := summaries[src.ID]
	if !ok {
		return
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
