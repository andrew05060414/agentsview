package parser

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMuseProviderDeclaresFileRawCapture(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentMuse)
	require.True(t, ok)
	assert.Equal(t, RawCaptureCapabilities{
		Support:  CapabilitySupported,
		Shape:    RawCaptureShapeFiles,
		Append:   RawCaptureAppendOne,
		Snapshot: RawCaptureSnapshotNone,
	}, factory.Capabilities().RawCapture)

	provider, ok := NewProvider(AgentMuse, ProviderConfig{Roots: []string{t.TempDir()}})
	require.True(t, ok)
	assert.Equal(t, factory.Capabilities().RawCapture, provider.Capabilities().RawCapture)
	_, implementsPlan := provider.(RawCaptureProvider)
	_, implementsStreaming := provider.(StreamingRawCaptureSourceProvider)
	assert.True(t, implementsPlan)
	assert.True(t, implementsStreaming)
}

func TestMuseProviderDiscoversSessionAndSubagentLogs(t *testing.T) {
	root, err := filepath.Abs(museFixtureRoot())
	require.NoError(t, err)
	provider, ok := NewProvider(AgentMuse, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var paths []string
	for _, source := range discovered {
		paths = append(paths, source.DisplayPath)
	}
	slices.Sort(paths)
	parentLog := filepath.Join(root, "2026", "09", "10", museFixtureParentID, museLogName)
	childLog := filepath.Join(root, "2026", "09", "10", museFixtureParentID,
		museSubagentDirName, museFixtureChildID, museLogName)
	assert.Equal(t, []string{parentLog, childLog}, paths)

	changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{
		Path: childLog, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: changed[0], Machine: "host"})
	require.NoError(t, err)
	assert.True(t, outcome.ForceReplace)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, "muse:"+museFixtureChildID, outcome.Results[0].Result.Session.ID)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "muse:" + museFixtureParentID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, parentLog, found.DisplayPath)
}

func TestMuseProviderIgnoresNonSessionFiles(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "2026", "09", "10", "sess-1")
	ignored := []string{
		filepath.Join(sessionDir, "approval-review", "review-1.jsonl"),
		filepath.Join(sessionDir, "approval-review", "review-1", museLogName),
		filepath.Join(sessionDir, "tool-outputs", "task-1", museLogName),
		filepath.Join(root, ".msp-view-v1", "sess-1", museLogName),
		filepath.Join(root, "2026", "9", "10", "sess-2", museLogName),
		filepath.Join(root, "2026", "09", "10", museLogName),
		filepath.Join(sessionDir, museSubagentDirName, museLogName),
		filepath.Join(sessionDir, "other.jsonl"),
	}
	for _, path := range ignored {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}
	nested := filepath.Join(sessionDir, museSubagentDirName, "child-1",
		museSubagentDirName, "grandchild-1", museLogName)
	require.NoError(t, os.MkdirAll(filepath.Dir(nested), 0o755))
	require.NoError(t, os.WriteFile(nested, []byte("{}\n"), 0o600))

	provider, ok := NewProvider(AgentMuse, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, nested, discovered[0].DisplayPath)
}

func TestMuseRawCapturePlansOnlyTheSessionLog(t *testing.T) {
	root := t.TempDir()
	sessionID := "0199a1b2-6c3d-7e4f-8a5b-000000000101"
	sessionDir := filepath.Join(root, "2026", "10", "24", sessionID)
	sessionLog := filepath.Join(sessionDir, museLogName)
	excluded := []string{
		filepath.Join(sessionDir, "tool-outputs", "task-1", museLogName),
		filepath.Join(sessionDir, "approval-review", "review-1", museLogName),
		filepath.Join(root, ".msp-view-v1", sessionID, museLogName),
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionLog), 0o755))
	require.NoError(t, os.WriteFile(sessionLog, []byte("{}\n"), 0o600))
	for _, path := range excluded {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}

	provider, ok := NewProvider(AgentMuse, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	discovery, err := DiscoverRawCaptureSources(t.Context(), provider)
	require.NoError(t, err)
	require.True(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	assert.Equal(t, sessionLog, discovery.Sources[0].DisplayPath)

	plan, supported, err := ResolveRawCapturePlan(t.Context(), provider, discovery.Sources[0])
	require.NoError(t, err)
	require.True(t, supported)
	assert.Equal(t, root, plan.ConfiguredRoot)
	assert.Equal(t, root, plan.CaptureRoot)
	assert.Equal(t, discovery.Sources[0].Key, plan.SourceKey)
	require.Len(t, plan.Entries, 1)
	assert.Equal(t, "2026/10/24/"+sessionID+"/"+museLogName, plan.Entries[0].Path)
	assert.Equal(t, sessionLog, plan.Entries[0].LocalPath)
	assert.True(t, plan.Entries[0].Appendable)
	assert.Empty(t, plan.SidecarRoots)
}
