package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

type statMeteredCountingProvider struct {
	parser.Provider
	fpCalls    atomic.Int64
	parseCalls atomic.Int64
	readBytes  atomic.Int64
}

func (p *statMeteredCountingProvider) DiscoverEach(
	ctx context.Context, yield func(parser.SourceRef) error,
) error {
	if sd, ok := p.Provider.(parser.StreamingDiscoverer); ok {
		return sd.DiscoverEach(ctx, yield)
	}
	sources, err := p.Provider.Discover(ctx)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if err := yield(src); err != nil {
			return err
		}
	}
	return nil
}

func (p *statMeteredCountingProvider) Fingerprint(
	ctx context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.fpCalls.Add(1)
	var meter parser.ContentReadMeter
	fp, err := p.Provider.Fingerprint(parser.WithContentReadMeter(ctx, &meter), source)
	p.readBytes.Add(meter.Bytes())
	return fp, err
}

func (p *statMeteredCountingProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.parseCalls.Add(1)
	return p.Provider.Parse(ctx, req)
}

func (p *statMeteredCountingProvider) ComputeMultiFileStatHash(path string) uint64 {
	if h, ok := p.Provider.(parser.MultiFileStatHasher); ok {
		return h.ComputeMultiFileStatHash(path)
	}
	return 0
}

type statMeteredCountingFactory struct {
	provider *statMeteredCountingProvider
}

func (f statMeteredCountingFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f statMeteredCountingFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f statMeteredCountingFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

func writeMinimalAntigravityDB(t *testing.T, dbPath string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	sdb, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer sdb.Close()
	_, err = sdb.ExecContext(t.Context(), `
		CREATE TABLE steps (
			idx integer, step_type integer NOT NULL DEFAULT 0,
			status integer NOT NULL DEFAULT 0,
			has_subtrajectory numeric NOT NULL DEFAULT false,
			metadata blob, error_details blob,
			permissions blob, task_details blob,
			render_info blob, step_payload blob,
			step_format integer NOT NULL DEFAULT 0,
			PRIMARY KEY (idx)
		)`)
	require.NoError(t, err)
}

func TestEngineStatHashRegistrationAntigravityAndGrok(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)

	assert.NotNil(t, engine.ProviderStatHasher(parser.AgentAntigravity))
	assert.NotNil(t, engine.ProviderStatHasher(parser.AgentGrok))
}

func TestSyncAntigravityWarmPassSkipsContentReadsViaStatHash(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = dbtest.AntigravityTitleID
	dbtest.WriteAntigravityTitleTranscript(t, root, "antigravity initial message")
	dbPath := filepath.Join(root, "conversations", id+".db")
	writeMinimalAntigravityDB(t, dbPath)
	planPath := filepath.Join(root, "brain", id, "plan.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o755))
	require.NoError(t, os.WriteFile(planPath, []byte("# Plan v1\n"), 0o644))

	newCountingEngine := func() (*Engine, *statMeteredCountingProvider) {
		innerFactory, ok := parser.ProviderFactoryByType(parser.AgentAntigravity)
		require.True(t, ok)
		inner := innerFactory.NewProvider(parser.ProviderConfig{
			Roots: []string{root}, Machine: "local",
		})
		counting := &statMeteredCountingProvider{Provider: inner}
		eng := NewEngine(t.Context(), database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentAntigravity: {root},
			},
			Machine: "local",
			ProviderFactories: []parser.ProviderFactory{
				statMeteredCountingFactory{provider: counting},
			},
		})
		t.Cleanup(eng.Close)
		return eng, counting
	}

	coldEngine, coldCounting := newCountingEngine()
	coldStats := coldEngine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, coldStats.Synced)
	require.Zero(t, coldStats.Failed)
	assert.EqualValues(t, 1, coldCounting.fpCalls.Load())
	assert.EqualValues(t, 1, coldCounting.parseCalls.Load())
	assert.Positive(t, coldCounting.readBytes.Load())

	storedDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentAntigravity, dbPath,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotZero(t, storedDigest)

	// A brand-new Engine simulates a fresh process: provider_freshness hits
	// with zero fingerprint calls and zero content bytes read.
	warmEngine, warmCounting := newCountingEngine()
	warmStats := warmEngine.SyncAll(t.Context(), nil)
	require.Zero(t, warmStats.Synced)
	require.Zero(t, warmStats.Failed)
	assert.Zero(t, warmCounting.fpCalls.Load())
	assert.Zero(t, warmCounting.parseCalls.Load())
	assert.Zero(t, warmCounting.readBytes.Load())

	// Modifying a companion artifact invalidates the stat hash and re-parses.
	require.NoError(t, os.WriteFile(planPath, []byte("# Plan v2\nUpdated body.\n"), 0o644))
	updateStats := warmEngine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, updateStats.Synced)
	require.Zero(t, updateStats.Failed)
	assert.EqualValues(t, 1, warmCounting.fpCalls.Load())
	assert.EqualValues(t, 1, warmCounting.parseCalls.Load())

	newDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentAntigravity, dbPath,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotEqual(t, storedDigest, newDigest)

	msgs, err := database.GetMessages(t.Context(), "antigravity:"+id, 0, 20, true)
	require.NoError(t, err)
	foundUpdated := false
	for _, msg := range msgs {
		if msg.Content == "[plan.md]\n# Plan v2\nUpdated body.\n" {
			foundUpdated = true
			break
		}
	}
	assert.True(t, foundUpdated, "re-parse must persist updated brain companion content")
}

func TestSyncGrokWarmPassSkipsContentReadsViaStatHash(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "33333333-3333-4333-8333-333333333333"
	sessionDir := filepath.Join(root, "cwd-a", id)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	summaryPath := filepath.Join(sessionDir, "summary.json")
	require.NoError(t, os.WriteFile(summaryPath, []byte(`{
		"summary": "Grok Warm Test",
		"firstPrompt": "hello grok",
		"createdAt": "2026-07-08T10:00:00Z"
	}`), 0o644))
	chatPath := filepath.Join(sessionDir, "chat_history.jsonl")
	require.NoError(t, os.WriteFile(chatPath, []byte(`{"role":"user","content":"hello grok"}`+"\n"), 0o644))

	newCountingEngine := func() (*Engine, *statMeteredCountingProvider) {
		innerFactory, ok := parser.ProviderFactoryByType(parser.AgentGrok)
		require.True(t, ok)
		inner := innerFactory.NewProvider(parser.ProviderConfig{
			Roots: []string{root}, Machine: "local",
		})
		counting := &statMeteredCountingProvider{Provider: inner}
		eng := NewEngine(t.Context(), database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentGrok: {root},
			},
			Machine: "local",
			ProviderFactories: []parser.ProviderFactory{
				statMeteredCountingFactory{provider: counting},
			},
		})
		t.Cleanup(eng.Close)
		return eng, counting
	}

	coldEngine, coldCounting := newCountingEngine()
	coldStats := coldEngine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, coldStats.Synced)
	require.Zero(t, coldStats.Failed)
	assert.EqualValues(t, 1, coldCounting.fpCalls.Load())
	assert.EqualValues(t, 1, coldCounting.parseCalls.Load())
	assert.Positive(t, coldCounting.readBytes.Load())

	storedDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentGrok, summaryPath,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotZero(t, storedDigest)

	warmEngine, warmCounting := newCountingEngine()
	warmStats := warmEngine.SyncAll(t.Context(), nil)
	require.Zero(t, warmStats.Synced)
	require.Zero(t, warmStats.Failed)
	assert.Zero(t, warmCounting.fpCalls.Load())
	assert.Zero(t, warmCounting.parseCalls.Load())
	assert.Zero(t, warmCounting.readBytes.Load())

	// Editing a companion transcript invalidates stat hash and triggers re-parse.
	require.NoError(t, os.WriteFile(chatPath, []byte(
		`{"role":"user","content":"hello grok"}`+"\n"+
			`{"role":"assistant","content":"reply v2"}`+"\n",
	), 0o644))
	updateStats := warmEngine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, updateStats.Synced)
	require.Zero(t, updateStats.Failed)
	assert.EqualValues(t, 1, warmCounting.fpCalls.Load())
	assert.EqualValues(t, 1, warmCounting.parseCalls.Load())

	newDigest, ok, err := database.GetProviderStatHash(
		t.Context(), parser.AgentGrok, summaryPath,
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotEqual(t, storedDigest, newDigest)
}

func TestSyncChangedPathsLockedUsesSourceFreshnessSnapshot(t *testing.T) {
	fx := newEngineFixture(t)
	p1 := fx.writeClaudeSession(t, "proj", "s1.jsonl", "one")
	p2 := fx.writeClaudeSession(t, "proj", "s2.jsonl", "two")
	require.Equal(t, 2, fx.engine.SyncAll(t.Context(), nil).Synced)
	fx.engine.Close()

	require.NoError(t, fx.db.CloseConnections(t.Context()))
	require.NoError(t, fx.db.Reopen())
	fx.engineWithEmitter(t.Context(), nil)
	t.Cleanup(fx.engine.Close)
	fx.engine.SetSoleArchiveWriter(true)

	var statHashReads, fileSizeReads atomic.Int32
	reader := fx.db.Reader().(interface {
		Conn(context.Context) (*sql.Conn, error)
	})
	var conns []*sql.Conn
	for range 4 {
		conn, err := reader.Conn(t.Context())
		require.NoError(t, err)
		conns = append(conns, conn)
		t.Cleanup(func() { _ = conn.Close() })
		require.NoError(t, conn.Raw(func(raw any) error {
			raw.(interface {
				RegisterAuthorizer(func(int, string, string, string) int)
			}).RegisterAuthorizer(func(op int, table, column, _ string) int {
				if op == sqlite3.SQLITE_READ {
					if table == "provider_freshness" && column == "stat_hash" {
						statHashReads.Add(1)
					}
					if table == "sessions" && column == "file_size" {
						fileSizeReads.Add(1)
					}
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
	}
	for _, conn := range conns {
		require.NoError(t, conn.Close())
	}

	require.NoError(t, fx.engine.SyncPathsContext(t.Context(), []string{p1, p2}))
	assert.EqualValues(t, 1, statHashReads.Load(), "SyncPaths must read stat_hash via one bulk snapshot")
	assert.EqualValues(t, 1, fileSizeReads.Load(), "SyncPaths must read file_size via one bulk snapshot")
}
