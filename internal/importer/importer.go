package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// ImportStats reports the outcome of an import operation.
type ImportStats struct {
	Imported int `json:"imported"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
}

// ImportCallbacks provides optional progress reporting.
type ImportCallbacks struct {
	// OnProgress fires after each conversation with current
	// cumulative stats.
	OnProgress func(ImportStats)
	// OnIndexing fires before the FTS index rebuild starts.
	OnIndexing func()
}

func (c *ImportCallbacks) progress(s ImportStats) {
	if c != nil && c.OnProgress != nil {
		c.OnProgress(s)
	}
}

func (c *ImportCallbacks) indexing() {
	if c != nil && c.OnIndexing != nil {
		c.OnIndexing()
	}
}

// ftsSuspender is optionally implemented by stores that
// support dropping and rebuilding FTS indexes.
type ftsSuspender interface {
	DropFTS(ctx context.Context) error
	RebuildFTS(ctx context.Context) error
}

// lazyFTS suspends FTS triggers on first call to suspend()
// and rebuilds on restore(). If suspend() is never called
// (no message work happened), restore() is a no-op. This
// avoids the expensive FTS rebuild when re-importing an
// unchanged archive.
type lazyFTS struct {
	sus        ftsSuspender
	dropped    bool
	onIndexing func()
}

func newLazyFTS(ctx context.Context,
	store db.Store, onIndexing func(),
) *lazyFTS {
	s, ok := store.(ftsSuspender)
	if !ok || !store.HasFTS(ctx) {
		return nil
	}
	return &lazyFTS{sus: s, onIndexing: onIndexing}
}

func (f *lazyFTS) suspend(ctx context.Context) {
	if f == nil || f.dropped {
		return
	}
	if err := f.sus.DropFTS(ctx); err != nil {
		log.Printf("import: drop FTS: %v", err)
		return
	}
	f.dropped = true
}

func (f *lazyFTS) restore(ctx context.Context) error {
	if f == nil || !f.dropped {
		return nil
	}
	if f.onIndexing != nil {
		f.onIndexing()
	}
	if err := f.sus.RebuildFTS(ctx); err != nil {
		return fmt.Errorf("rebuilding FTS index: %w", err)
	}
	return nil
}

// ImportClaudeAI reads a Claude.ai conversations.json export
// and upserts each conversation into the store. Existing
// sessions are updated (messages replaced); user-renamed
// display names are preserved. Excluded (deleted) sessions
// are counted as skipped.
func ImportClaudeAI(
	ctx context.Context,
	store db.Store,
	r io.Reader,
	cb *ImportCallbacks,
	machine ...string,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(ctx, store, cb.indexing)
	defer func() {
		if err := fts.restore(ctx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	provider, ok := parser.NewProvider(
		parser.AgentClaudeAI, parser.ProviderConfig{},
	)
	if !ok {
		return stats, errors.New("claude.ai provider unavailable")
	}
	exporter, ok := provider.(parser.ClaudeAIExportParser)
	if !ok {
		return stats, errors.New("claude.ai provider does not support exports")
	}

	err := exporter.ParseClaudeAIExport(r, func(
		result parser.ParseResult,
	) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		result.Session.Machine = resolvedImportMachine(
			result.Session.Machine, machine,
		)
		status, err := upsertConversation(
			ctx, store, result, fts,
		)
		if err != nil {
			stats.Errors++
			log.Printf(
				"import: skipping %s: %v",
				result.Session.ID, err,
			)
			cb.progress(stats)
			return nil
		}

		switch status {
		case importNew:
			stats.Imported++
		case importUpdated:
			stats.Updated++
		case importSkipped:
			stats.Skipped++
		}

		cb.progress(stats)
		return nil
	})

	retErr = err
	return
}

type importStatus int

const (
	importNew importStatus = iota
	importUpdated
	importSkipped
)

func upsertConversation(
	ctx context.Context,
	store db.Store,
	result parser.ParseResult,
	fts *lazyFTS,
) (importStatus, error) {
	s := result.Session

	msgs := make([]db.Message, len(result.Messages))
	for i, m := range result.Messages {
		msgs[i] = db.Message{
			SessionID:     s.ID,
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			Timestamp:     m.Timestamp.UTC().Format(time.RFC3339Nano),
			ContentLength: m.ContentLength,
		}
	}

	existing, err := store.GetSession(ctx, s.ID)
	if err != nil {
		return importNew, fmt.Errorf("checking session: %w", err)
	}
	isNew := existing == nil

	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		FirstMessage:     strPtr(s.FirstMessage),
		SessionName:      db.ParsedSessionName(s),
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}
	db.ApplyParsedSessionIdentity(&sess, s)

	if err := store.UpsertSession(ctx, sess); err != nil {
		if errors.Is(err, db.ErrSessionExcluded) {
			return importSkipped, nil
		}
		return importNew, fmt.Errorf("upserting session: %w", err)
	}

	// Bump local_modified_at so incremental PG push picks up session_name
	// changes even when the skip path below returns importSkipped (message
	// count unchanged) and ReplaceSessionMessages is never called.
	if localDB, ok := store.(*db.DB); ok {
		if err := localDB.BumpLocalModifiedAt(ctx, s.ID); err != nil {
			log.Printf("import: bumping local_modified_at for %s: %v", s.ID, err)
		}
	}

	// Skip expensive message replacement when the conversation
	// has not changed since the last import. Compare both
	// message count and ended_at (source updated_at) to detect
	// content/metadata changes even when count is unchanged.
	if !isNew && existing != nil && existing.MessageCount == s.MessageCount {
		newEnd := timeStr(s.EndedAt)
		if ptrEqual(existing.EndedAt, newEnd) {
			existingMsgs, err := store.GetAllMessages(ctx, s.ID)
			if err != nil {
				return importNew,
					fmt.Errorf("loading existing messages: %w", err)
			}
			if sameMessages(existingMsgs, msgs) {
				return importSkipped, nil
			}
		}
	}

	// Suspend FTS before first message-changing operation to
	// avoid per-row trigger overhead during bulk work.
	fts.suspend(ctx)

	if err := store.ReplaceSessionMessages(ctx, s.ID, msgs); err != nil {
		return importNew, fmt.Errorf("replacing messages: %w", err)
	}

	if isNew {
		return importNew, nil
	}
	return importUpdated, nil
}

// assetResolverAdapter bridges the importer's AssetIndex / CopyAsset
// pair to the parser.AssetResolver interface.
type assetResolverAdapter struct {
	index     AssetIndex
	assetsDir string
}

func (a *assetResolverAdapter) Resolve(
	pointer string,
) (string, bool) {
	return a.index.Resolve(pointer)
}

func (a *assetResolverAdapter) Copy(
	srcPath string,
) (string, error) {
	return assets.CopyAsset(srcPath, a.assetsDir)
}

// ImportChatGPT reads a ChatGPT export directory (containing
// conversations-*.json files) and imports each conversation into
// the store. Existing sessions are updated only when the export
// contains the complete archived message prefix followed by new messages.
func ImportChatGPT(
	ctx context.Context,
	store db.Store,
	dir string,
	assetsDir string,
	cb *ImportCallbacks,
	machine ...string,
) (stats ImportStats, retErr error) {
	fts := newLazyFTS(ctx, store, cb.indexing)
	defer func() {
		if err := fts.restore(ctx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	index := BuildAssetIndex(dir)
	resolver := &assetResolverAdapter{
		index:     index,
		assetsDir: assetsDir,
	}

	provider, ok := parser.NewProvider(
		parser.AgentChatGPT, parser.ProviderConfig{},
	)
	if !ok {
		return stats, errors.New("chatgpt provider unavailable")
	}
	exporter, ok := provider.(parser.ChatGPTExportParser)
	if !ok {
		return stats, errors.New("chatgpt provider does not support exports")
	}

	err := exporter.ParseChatGPTExport(dir, resolver,
		func(result parser.ParseResult) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			s := result.Session
			s.Machine = resolvedImportMachine(s.Machine, machine)
			if !strings.HasPrefix(s.ID, "chatgpt:") {
				stats.Errors++
				log.Printf("import: refusing ChatGPT conversation with unexpected session ID %q", s.ID)
				cb.progress(stats)
				return nil
			}
			msgs := chatGPTMessages(s.ID, result.Messages)

			existing, err := store.GetSession(ctx, s.ID)
			if err != nil {
				stats.Errors++
				log.Printf(
					"import: skipping %s: %v", s.ID, err,
				)
				cb.progress(stats)
				return nil
			}
			if existing != nil {
				if existing.Agent != string(parser.AgentChatGPT) {
					stats.Errors++
					log.Printf("import: refusing ChatGPT conversation %s: existing session belongs to %q", s.ID, existing.Agent)
					cb.progress(stats)
					return nil
				}
				existingMsgs, err := store.GetAllMessages(ctx, s.ID)
				if err != nil {
					stats.Errors++
					log.Printf("import: loading existing ChatGPT messages for %s: %v", s.ID, err)
					cb.progress(stats)
					return nil
				}
				if len(msgs) < len(existingMsgs) {
					stats.Errors++
					log.Printf("import: refusing ChatGPT conversation %s: export has %d messages, archive has %d", s.ID, len(msgs), len(existingMsgs))
					cb.progress(stats)
					return nil
				}
				if !sameMessages(existingMsgs, msgs[:len(existingMsgs)]) {
					stats.Errors++
					log.Printf("import: refusing ChatGPT conversation %s: export history diverges from archive", s.ID)
					cb.progress(stats)
					return nil
				}
				if len(msgs) == len(existingMsgs) {
					if localDB, ok := store.(*db.DB); ok {
						if err := localDB.RefreshSessionName(ctx, s.ID, db.ParsedSessionName(s)); err != nil {
							stats.Errors++
							log.Printf("import: refreshing session_name for %s: %v", s.ID, err)
							cb.progress(stats)
							return nil
						}
					}
					stats.Skipped++
					cb.progress(stats)
					return nil
				}
				// Keep the archived prefix as-is. ChatGPT exports do not always
				// carry every normalized field stored on earlier messages; using
				// parsed prefix rows here could silently discard that metadata.
				copy(msgs, existingMsgs)
				sess := chatGPTSession(s)
				fts.suspend(ctx)
				if err := writeChatGPTSession(ctx, store, sess, msgs); err != nil {
					stats.Errors++
					log.Printf("import: updating ChatGPT conversation %s: %v", s.ID, err)
					cb.progress(stats)
					return nil
				}
				stats.Updated++
				cb.progress(stats)
				return nil
			}

			sess := chatGPTSession(s)
			fts.suspend(ctx)
			if err := writeChatGPTSession(ctx, store, sess, msgs); err != nil {
				if errors.Is(err, db.ErrSessionExcluded) {
					stats.Skipped++
					cb.progress(stats)
					return nil
				}
				stats.Errors++
				log.Printf(
					"import: skipping %s: %v", s.ID, err,
				)
				cb.progress(stats)
				return nil
			}

			stats.Imported++
			cb.progress(stats)
			return nil
		},
	)

	retErr = err
	return
}

func chatGPTSession(s parser.ParsedSession) db.Session {
	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		FirstMessage:     strPtr(s.FirstMessage),
		SessionName:      db.ParsedSessionName(s),
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}
	db.ApplyParsedSessionIdentity(&sess, s)
	return sess
}

func chatGPTMessages(sessionID string, parsed []parser.ParsedMessage) []db.Message {
	msgs := make([]db.Message, len(parsed))
	for i, m := range parsed {
		msgs[i] = db.Message{
			SessionID:     sessionID,
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			Timestamp:     m.Timestamp.UTC().Format(time.RFC3339Nano),
			HasThinking:   m.HasThinking,
			HasToolUse:    m.HasToolUse,
			ContentLength: m.ContentLength,
			IsSystem:      m.IsSystem,
			Model:         m.Model,
			ToolCalls:     convertToolCalls(sessionID, m.ToolCalls),
		}
	}
	return msgs
}

func writeChatGPTSession(ctx context.Context, store db.Store, sess db.Session, msgs []db.Message) error {
	result, err := store.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session:                    sess,
		Messages:                   msgs,
		SkipSignalUpdates:          true,
		ReplaceMessages:            true,
		RejectMessageCountDecrease: true,
	}})
	if err != nil {
		return err
	}
	if result.ExcludedSessions > 0 {
		return db.ErrSessionExcluded
	}
	if result.FailedSessions > 0 && len(result.Errors) > 0 {
		return result.Errors[0]
	}
	return nil
}

func resolvedImportMachine(current string, override []string) string {
	if len(override) > 0 && strings.TrimSpace(override[0]) != "" {
		return override[0]
	}
	return current
}

func ptrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func sameMessages(existing, incoming []db.Message) bool {
	if len(existing) != len(incoming) {
		return false
	}
	for i := range existing {
		if existing[i].Ordinal != incoming[i].Ordinal ||
			existing[i].Role != incoming[i].Role ||
			existing[i].Content != incoming[i].Content ||
			existing[i].Timestamp != incoming[i].Timestamp ||
			existing[i].ContentLength != incoming[i].ContentLength {
			return false
		}
	}
	return true
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timeStr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func convertToolCalls(
	sessionID string, parsed []parser.ParsedToolCall,
) []db.ToolCall {
	if len(parsed) == 0 {
		return nil
	}
	calls := make([]db.ToolCall, len(parsed))
	for i, tc := range parsed {
		filePath := tc.FilePath
		if filePath == "" {
			filePath = parser.ResolveFilePathFromJSON(tc.InputJSON)
		}
		calls[i] = db.ToolCall{
			SessionID: sessionID,
			ToolName:  tc.ToolName,
			Category:  tc.Category,
			ToolUseID: tc.ToolUseID,
			InputJSON: tc.InputJSON,
			FilePath:  filePath,
			SkillName: tc.SkillName,
		}
		// Map execution output from ResultEvents to
		// ResultContent for display in the UI.
		for _, ev := range tc.ResultEvents {
			if ev.Content != "" {
				calls[i].ResultContent = ev.Content
				calls[i].ResultContentLength = len(ev.Content)
				break
			}
		}
	}
	return calls
}
