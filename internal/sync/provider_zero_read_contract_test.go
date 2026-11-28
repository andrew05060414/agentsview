package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// contractMeteredProviderFactory wraps a real ProviderFactory while forwarding
// the full ProviderConfig (including MetadataDirs injected by
// ConfigureProviderFactory) and aggregating Fingerprint/Parse/ContentReadMeter
// metrics across all constructed provider instances.
type contractMeteredProviderFactory struct {
	inner      parser.ProviderFactory
	fpCalls    *atomic.Int64
	parseCalls *atomic.Int64
	readBytes  *atomic.Int64
}

func newContractMeteredFactory(
	t *testing.T, agent parser.AgentType,
) *contractMeteredProviderFactory {
	t.Helper()
	inner, ok := parser.ProviderFactoryByType(agent)
	require.True(t, ok, "provider factory %s must be registered", agent)
	return &contractMeteredProviderFactory{
		inner:      inner,
		fpCalls:    new(atomic.Int64),
		parseCalls: new(atomic.Int64),
		readBytes:  new(atomic.Int64),
	}
}

func (f *contractMeteredProviderFactory) Definition() parser.AgentDef {
	return f.inner.Definition()
}

func (f *contractMeteredProviderFactory) Capabilities() parser.Capabilities {
	return f.inner.Capabilities()
}

func (f *contractMeteredProviderFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &contractMeteredProvider{
		Provider:   f.inner.NewProvider(cfg),
		fpCalls:    f.fpCalls,
		parseCalls: f.parseCalls,
		readBytes:  f.readBytes,
	}
}

func (f *contractMeteredProviderFactory) Reset() {
	f.fpCalls.Store(0)
	f.parseCalls.Store(0)
	f.readBytes.Store(0)
}

type contractMeteredProvider struct {
	parser.Provider
	fpCalls    *atomic.Int64
	parseCalls *atomic.Int64
	readBytes  *atomic.Int64
}

func (p *contractMeteredProvider) DiscoverEach(
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

func (p *contractMeteredProvider) Fingerprint(
	ctx context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.fpCalls.Add(1)
	var meter parser.ContentReadMeter
	fp, err := p.Provider.Fingerprint(parser.WithContentReadMeter(ctx, &meter), source)
	p.readBytes.Add(meter.Bytes())
	return fp, err
}

func (p *contractMeteredProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.parseCalls.Add(1)
	return p.Provider.Parse(ctx, req)
}

func (p *contractMeteredProvider) ComputeMultiFileStatHash(path string) uint64 {
	if h, ok := p.Provider.(parser.MultiFileStatHasher); ok {
		return h.ComputeMultiFileStatHash(path)
	}
	return 0
}

func (p *contractMeteredProvider) EffectiveMtime(path string, info os.FileInfo) int64 {
	if m, ok := p.Provider.(interface {
		EffectiveMtime(string, os.FileInfo) int64
	}); ok {
		return m.EffectiveMtime(path, info)
	}
	return info.ModTime().UnixNano()
}

// TestMultiFileStatHashProvidersRegisteredAndWiredInEngine fails if any
// registered provider declares Source.MultiFileStatHash = CapabilitySupported
// without being wired into Engine's stat hasher table, providerStatFreshnessMtime,
// and providerStatDigestEligible (both local and remote pathRewriter guards).
func TestMultiFileStatHashProvidersRegisteredAndWiredInEngine(t *testing.T) {
	auditedProviders := map[parser.AgentType]struct{}{
		parser.AgentAntigravity:     {},
		parser.AgentAntigravityCLI:  {},
		parser.AgentAugureCode:      {},
		parser.AgentClaude:          {},
		parser.AgentCodebuff:        {},
		parser.AgentCodex:           {},
		parser.AgentCursor:          {},
		parser.AgentDeepSeekHarness: {},
		parser.AgentEvener:          {},
		parser.AgentGrok:            {},
		parser.AgentInterpreter:     {},
		parser.AgentMusePersonal:    {},
		parser.AgentTraeX:           {},
		parser.AgentVSCodeCopilot:   {},
	}

	tmp := t.TempDir()
	cursorRoot := filepath.Join(tmp, ".cursor")
	cursorProjects := filepath.Join(cursorRoot, "projects")
	cursorChats := filepath.Join(cursorRoot, "chats")
	vscRoot := filepath.Join(tmp, "vscode")
	dshRoot := filepath.Join(tmp, "deepseek-harness")

	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCursor:          {cursorProjects},
			parser.AgentVSCodeCopilot:   {vscRoot},
			parser.AgentDeepSeekHarness: {dshRoot},
		},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			parser.AgentCursor: {cursorProjects: {cursorChats}},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	remoteEngine := NewEngine(t.Context(), database, EngineConfig{
		Machine:      "local",
		PathRewriter: func(p string) string { return "remote:" + p },
	})
	t.Cleanup(remoteEngine.Close)

	probeFile := filepath.Join(tmp, "probe.jsonl")
	require.NoError(t, os.WriteFile(probeFile, []byte("probe\n"), 0o600))
	probeInfo, err := os.Stat(probeFile)
	require.NoError(t, err)

	for _, factory := range parser.ProviderFactories() {
		agent := factory.Definition().Type
		caps := factory.Capabilities()
		if caps.Source.MultiFileStatHash != parser.CapabilitySupported {
			continue
		}
		_, audited := auditedProviders[agent]
		assert.True(t, audited,
			"provider %s declares MultiFileStatHash=Supported; audit and wire it in engine.go (providerStatFreshnessMtime, providerStatDigestEligible) and add it to auditedProviders",
			agent)
		assert.NotNil(t, engine.ProviderStatHasher(agent),
			"provider %s declares MultiFileStatHash=Supported but was not registered in providerStatHashers",
			agent)
		assert.True(t, engine.providerStatDigestEligible(agent),
			"local engine must allow stat digests for %s", agent)
		assert.Equal(t, probeInfo.ModTime().UnixNano(),
			engine.providerStatFreshnessMtime(agent, probeFile, probeInfo),
			"providerStatFreshnessMtime baseline check failed for %s", agent)
	}

	// Remote engines conservatively withhold stat digests for the newly governed
	// providers, matching antigravity and grok.
	for _, agent := range []parser.AgentType{
		parser.AgentCursor,
		parser.AgentVSCodeCopilot,
		parser.AgentDeepSeekHarness,
	} {
		assert.False(t, remoteEngine.providerStatDigestEligible(agent),
			"remote pathRewriter engine must withhold stat digest for %s", agent)
	}

	// Verify Cursor's registered stat hasher received real MetadataDirs and
	// detects store.db companion changes, and providerStatFreshnessMtime folds
	// companion mtimes for Cursor, VSCode Copilot, and DeepSeek Harness.
	t.Run("cursor metadata and companion mtime", func(t *testing.T) {
		const sid = "11111111-2222-4333-8444-555555555555"
		transcript := filepath.Join(cursorProjects, "Users-demo-app", "agent-transcripts", sid+".jsonl")
		storePath := filepath.Join(cursorChats, "ws1", sid, "store.db")
		require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Dir(storePath), 0o755))
		require.NoError(t, os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":"hi"}}`+"\n"), 0o644))
		t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(transcript, t1, t1))

		sdb, err := sql.Open("sqlite3", storePath)
		require.NoError(t, err)
		_, err = sdb.ExecContext(t.Context(), `CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT); CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB);`)
		require.NoError(t, err)
		require.NoError(t, sdb.Close())
		t2 := t1.Add(5 * time.Minute)
		require.NoError(t, os.Chtimes(storePath, t2, t2))

		hasher := engine.ProviderStatHasher(parser.AgentCursor)
		require.NotNil(t, hasher)
		digestBefore := hasher.ComputeMultiFileStatHash(transcript)
		require.NotZero(t, digestBefore, "Cursor stat hasher must use configured MetadataDirs")

		tInfo, err := os.Stat(transcript)
		require.NoError(t, err)
		assert.Equal(t, t2.UnixNano(), engine.providerStatFreshnessMtime(parser.AgentCursor, transcript, tInfo),
			"Cursor providerStatFreshnessMtime must include store.db mtime")

		t3 := t2.Add(time.Minute)
		require.NoError(t, os.Chtimes(storePath, t3, t3))
		digestAfter := hasher.ComputeMultiFileStatHash(transcript)
		assert.NotEqual(t, digestBefore, digestAfter, "Cursor stat hasher must reflect store.db change")
	})

	t.Run("vscode-copilot companion mtime", func(t *testing.T) {
		wsDir := filepath.Join(vscRoot, "workspaceStorage", "ws1")
		chatPath := filepath.Join(wsDir, "chatSessions", "s1.jsonl")
		wsPath := filepath.Join(wsDir, "workspace.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(chatPath), 0o755))
		require.NoError(t, os.WriteFile(chatPath, []byte("{}\n"), 0o644))
		require.NoError(t, os.WriteFile(wsPath, []byte(`{"folder":"file:///app"}`), 0o644))
		t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		t2 := t1.Add(5 * time.Minute)
		require.NoError(t, os.Chtimes(chatPath, t1, t1))
		require.NoError(t, os.Chtimes(wsPath, t2, t2))
		chatInfo, err := os.Stat(chatPath)
		require.NoError(t, err)
		assert.Equal(t, t2.UnixNano(), engine.providerStatFreshnessMtime(parser.AgentVSCodeCopilot, chatPath, chatInfo))
	})

	t.Run("deepseek-harness companion mtime", func(t *testing.T) {
		sessDir := filepath.Join(dshRoot, "--proj--", "s1")
		mainPath := filepath.Join(sessDir, "session.jsonl")
		altPath := filepath.Join(sessDir, "session.jsonl.zstd")
		require.NoError(t, os.MkdirAll(sessDir, 0o755))
		require.NoError(t, os.WriteFile(mainPath, []byte("{}\n"), 0o644))
		require.NoError(t, os.WriteFile(altPath, []byte("z"), 0o644))
		t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		t2 := t1.Add(5 * time.Minute)
		require.NoError(t, os.Chtimes(mainPath, t1, t1))
		require.NoError(t, os.Chtimes(altPath, t2, t2))
		mainInfo, err := os.Stat(mainPath)
		require.NoError(t, err)
		assert.Equal(t, t2.UnixNano(), engine.providerStatFreshnessMtime(parser.AgentDeepSeekHarness, mainPath, mainInfo))
	})
}

// TestLocalProvidersWarmSyncZeroReadContract runs cold sync -> warm sync (new
// Engine over the same DB) -> companion/input mutation -> re-sync for every
// governed local provider and enforces zero ContentReadMeter bytes on warm sync.
func TestLocalProvidersWarmSyncZeroReadContract(t *testing.T) {
	type providerFixture struct {
		agent               parser.AgentType
		setup               func(t *testing.T) (roots []string, metadata map[string][]string, mutate func(t *testing.T))
		expectSynced        int
		expectColdReadBytes bool
	}

	cases := []providerFixture{
		{
			agent: parser.AgentAntigravity,
			setup: func(t *testing.T) ([]string, map[string][]string, func(t *testing.T)) {
				t.Helper()
				root := t.TempDir()
				const id = dbtest.AntigravityTitleID
				dbtest.WriteAntigravityTitleTranscript(t, root, "antigravity zero read")
				writeMinimalAntigravityDB(t, filepath.Join(root, "conversations", id+".db"))
				planPath := filepath.Join(root, "brain", id, "plan.md")
				require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o755))
				require.NoError(t, os.WriteFile(planPath, []byte("# Plan v1\n"), 0o644))
				return []string{root}, nil, func(t *testing.T) {
					t.Helper()
					require.NoError(t, os.WriteFile(planPath, []byte("# Plan v2\nUpdated companion\n"), 0o644))
				}
			},
			expectSynced:        1,
			expectColdReadBytes: true,
		},
		{
			agent: parser.AgentAntigravityCLI,
			setup: func(t *testing.T) ([]string, map[string][]string, func(t *testing.T)) {
				t.Helper()
				root := t.TempDir()
				const id = "33333333-4444-5555-6666-777777777777"
				convDir := filepath.Join(root, "conversations")
				require.NoError(t, os.MkdirAll(convDir, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "history.jsonl"),
					[]byte(`{"conversationId":"`+id+`","workspace":"/workspace/agy-cli","timestamp":1716244800000,"display":"CLI prompt"}`+"\n"),
					0o644,
				))
				require.NoError(t, os.WriteFile(filepath.Join(convDir, id+".pb"), []byte("dummy-pb"), 0o644))
				trajPath := filepath.Join(convDir, id+".trajectory.json")
				writeTraj := func(msg string) {
					body := fmt.Sprintf(`{
						"trajectoryId": %q,
						"steps": [{
							"type": "CORTEX_STEP_TYPE_USER_INPUT",
							"status": "STATUS_COMPLETED",
							"metadata": {"createdAt": "2026-05-20T22:40:00Z"},
							"userInput": {"userResponse": %q}
						}]
					}`, id, msg)
					require.NoError(t, os.WriteFile(trajPath, []byte(body), 0o644))
				}
				writeTraj("CLI v1")
				return []string{root}, nil, func(t *testing.T) {
					t.Helper()
					writeTraj("CLI v2 updated")
				}
			},
			expectSynced:        1,
			expectColdReadBytes: true,
		},
		{
			agent: parser.AgentGrok,
			setup: func(t *testing.T) ([]string, map[string][]string, func(t *testing.T)) {
				t.Helper()
				root := t.TempDir()
				const id = "44444444-4444-4444-8444-444444444444"
				sessionDir := filepath.Join(root, "cwd-a", id)
				require.NoError(t, os.MkdirAll(sessionDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "summary.json"), []byte(`{
					"summary": "Grok Contract",
					"firstPrompt": "hello grok",
					"createdAt": "2026-07-08T10:00:00Z"
				}`), 0o644))
				chatPath := filepath.Join(sessionDir, "chat_history.jsonl")
				require.NoError(t, os.WriteFile(chatPath, []byte(`{"role":"user","content":"hello grok"}`+"\n"), 0o644))
				return []string{root}, nil, func(t *testing.T) {
					t.Helper()
					require.NoError(t, os.WriteFile(chatPath, []byte(
						`{"role":"user","content":"hello grok"}`+"\n"+
							`{"role":"assistant","content":"reply v2"}`+"\n",
					), 0o644))
				}
			},
			expectSynced:        1,
			expectColdReadBytes: true,
		},
		{
			agent: parser.AgentCursor,
			setup: func(t *testing.T) ([]string, map[string][]string, func(t *testing.T)) {
				t.Helper()
				cursorRoot := filepath.Join(t.TempDir(), ".cursor")
				projectsRoot := filepath.Join(cursorRoot, "projects")
				chatsRoot := filepath.Join(cursorRoot, "chats")
				const sid = "55555555-2222-4333-8444-555555555555"
				transcript := filepath.Join(projectsRoot, "Users-demo-app", "agent-transcripts", sid+".jsonl")
				storePath := filepath.Join(chatsRoot, "ws1", sid, "store.db")
				require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Dir(storePath), 0o755))
				require.NoError(t, os.WriteFile(transcript, []byte(
					`{"role":"user","message":{"content":"<user_query>Cursor v1</user_query>"}}`+"\n"+
						`{"role":"assistant","message":{"content":"Answer v1"}}`+"\n",
				), 0o644))
				sdb, err := sql.Open("sqlite3", storePath)
				require.NoError(t, err)
				_, err = sdb.ExecContext(t.Context(), `
					CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
					CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB);
					INSERT INTO meta VALUES ('0', 'not-hex-v1');
				`)
				require.NoError(t, err)
				require.NoError(t, sdb.Close())

				meta := map[string][]string{projectsRoot: {chatsRoot}}
				return []string{projectsRoot}, meta, func(t *testing.T) {
					t.Helper()
					// Mutate the companion store.db (leaving transcript untouched) to prove
					// Cursor's MultiFileStatHash tracks MetadataDirs companions too.
					sdb, err := sql.Open("sqlite3", storePath)
					require.NoError(t, err)
					_, err = sdb.ExecContext(t.Context(), `UPDATE meta SET value = 'not-hex-v2-changed' WHERE key = '0'`)
					require.NoError(t, err)
					require.NoError(t, sdb.Close())
				}
			},
			expectSynced:        1,
			expectColdReadBytes: true,
		},
		{
			agent: parser.AgentVSCodeCopilot,
			setup: func(t *testing.T) ([]string, map[string][]string, func(t *testing.T)) {
				t.Helper()
				root := t.TempDir()
				wsDir := filepath.Join(root, "workspaceStorage", "ws1")
				chatPath := filepath.Join(wsDir, "chatSessions", "66666666-2222-4333-8444-666666666666.jsonl")
				wsPath := filepath.Join(wsDir, "workspace.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(chatPath), 0o755))
				session := `{"version":1,"sessionId":"66666666-2222-4333-8444-666666666666","creationDate":1704103200000,"lastMessageDate":1704103260000,"requests":[{"requestId":"r1","message":{"text":"hello vsc"},"response":[{"value":"hi vsc"}],"timestamp":1704103200000}]}`
				require.NoError(t, os.WriteFile(chatPath, []byte(`{"kind":0,"v":`+session+`}`+"\n"), 0o644))
				require.NoError(t, os.WriteFile(wsPath, []byte(`{"folder":"file:///Users/demo/project-one"}`), 0o644))
				return []string{root}, nil, func(t *testing.T) {
					t.Helper()
					require.NoError(t, os.WriteFile(wsPath, []byte(`{"folder":"file:///Users/demo/project-two-updated"}`), 0o644))
				}
			},
			expectSynced:        1,
			expectColdReadBytes: true,
		},
		{
			agent: parser.AgentDeepSeekHarness,
			setup: func(t *testing.T) ([]string, map[string][]string, func(t *testing.T)) {
				t.Helper()
				root := t.TempDir()
				dir := filepath.Join(root, "--workspace-example--", "contract-session")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				path := filepath.Join(dir, "session.jsonl")
				writeLog := func(answer string) {
					header := map[string]any{
						"type": "session", "version": 0, "id": "contract-session",
						"createdAt": 1700000000000, "cwd": "/workspace/example",
						"delegationDepth": 0,
					}
					events := []any{
						map[string]any{"type": "turn/start", "seq": 0, "time": 1700000000001, "data": map[string]any{"turn": 1}},
						map[string]any{"type": "step/start", "seq": 1, "time": 1700000000002, "data": map[string]any{"turn": 1, "step": 1}},
						map[string]any{
							"type": "user/message", "seq": 2, "time": 1700000000003, "surfaceOp": "append",
							"data": map[string]any{"id": "u1", "role": "user", "source": map[string]any{"kind": "user"}, "content": []any{map[string]any{"type": "text", "text": "hello dsh"}}},
						},
						map[string]any{
							"type": "assistant/message", "seq": 3, "time": 1700000000004, "surfaceOp": "append",
							"data": map[string]any{
								"turn": 1, "step": 1,
								"message": map[string]any{
									"id": "a1", "role": "assistant",
									"source":  map[string]any{"kind": "model", "provider": "deepseek", "model": "deepseek-chat"},
									"content": []any{map[string]any{"type": "text", "text": answer}},
								},
							},
						},
						map[string]any{"type": "step/end", "seq": 4, "time": 1700000000005, "data": map[string]any{"turn": 1, "step": 1}},
						map[string]any{"type": "turn/end", "seq": 5, "time": 1700000000006, "data": map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}},
					}
					var buf bytes.Buffer
					for _, rec := range append([]any{header}, events...) {
						line, err := json.Marshal(rec)
						require.NoError(t, err)
						buf.Write(line)
						buf.WriteByte('\n')
					}
					require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
				}
				writeLog("answer v1")
				return []string{root}, nil, func(t *testing.T) {
					t.Helper()
					writeLog("answer v2 updated")
				}
			},
			expectSynced:        1,
			expectColdReadBytes: false, // DeepSeek Harness Fingerprint is stat-only.
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.agent), func(t *testing.T) {
			database := openTestDB(t)
			roots, metadata, mutate := tc.setup(t)
			factory := newContractMeteredFactory(t, tc.agent)

			newEngine := func() *Engine {
				cfg := EngineConfig{
					AgentDirs:         map[parser.AgentType][]string{tc.agent: roots},
					Machine:           "local",
					ProviderFactories: []parser.ProviderFactory{factory},
				}
				if len(metadata) > 0 {
					cfg.ProviderMetadata = map[parser.AgentType]map[string][]string{
						tc.agent: metadata,
					}
				}
				eng := NewEngine(t.Context(), database, cfg)
				t.Cleanup(eng.Close)
				return eng
			}

			// 1. Cold sync: parses once and populates provider_freshness.
			coldEngine := newEngine()
			require.NotNil(t, coldEngine.ProviderStatHasher(tc.agent),
				"%s must have a registered stat hasher", tc.agent)
			coldStats := coldEngine.SyncAll(t.Context(), nil)
			require.Equal(t, tc.expectSynced, coldStats.Synced)
			require.Zero(t, coldStats.Failed)
			assert.Positive(t, factory.fpCalls.Load())
			assert.EqualValues(t, tc.expectSynced, factory.parseCalls.Load())
			if tc.expectColdReadBytes {
				assert.Positive(t, factory.readBytes.Load())
			}

			// 2. Warm sync on a brand-new Engine: 0 bytes read via ContentReadMeter.
			factory.Reset()
			warmEngine := newEngine()
			warmStats := warmEngine.SyncAll(t.Context(), nil)
			require.Zero(t, warmStats.Synced)
			require.Zero(t, warmStats.Failed)
			assert.Zero(t, factory.fpCalls.Load(), "warm sync must skip Fingerprint")
			assert.Zero(t, factory.parseCalls.Load(), "warm sync must skip Parse")
			assert.Zero(t, factory.readBytes.Load(), "warm sync must read 0 bytes via ContentReadMeter")

			// 3. Mutate an input/companion file: triggers re-parse immediately.
			factory.Reset()
			mutate(t)
			updateStats := warmEngine.SyncAll(t.Context(), nil)
			require.Equal(t, tc.expectSynced, updateStats.Synced)
			require.Zero(t, updateStats.Failed)
			assert.Positive(t, factory.fpCalls.Load())
			assert.EqualValues(t, tc.expectSynced, factory.parseCalls.Load())

			// 4. Subsequent warm sync returns to 0 bytes read.
			factory.Reset()
			settledEngine := newEngine()
			settledStats := settledEngine.SyncAll(t.Context(), nil)
			require.Zero(t, settledStats.Synced)
			require.Zero(t, settledStats.Failed)
			assert.Zero(t, factory.readBytes.Load(), "post-update warm sync must read 0 bytes")
		})
	}
}
