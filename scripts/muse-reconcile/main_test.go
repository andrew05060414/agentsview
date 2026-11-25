package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileTranscriptCounts(t *testing.T) {
	root := t.TempDir()
	path := writeTranscript(t, root, 1, 1, message(0)+message(2)+message(7))
	writeTranscript(t, root, 2, 2, "")
	writeTranscript(t, root, 3, 1, `{"type":"item"`)
	writeTranscript(t, root, 4, 1, "invalid json\n"+message(1))

	var output, diagnostics bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), root, &output, &diagnostics))
	assert.JSONEq(t, `{"parsed":2,"skipped":1,"failed":1,"messages":4,"malformed_lines":1}`, output.String())
	assert.Empty(t, diagnostics.String())

	// A compacted snapshot keeps the same path and drops a prefix.
	writeTranscript(t, root, 1, 1, message(7)+`{"type":"compaction_checkpoint","created_at":1790578141,"checkpoint_seq":8,"summary":"example"}`+"\n")
	updated, err := reconcile(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, counts{Parsed: 2, Skipped: 1, Failed: 1, Messages: 3, MalformedLines: 1}, updated)
	require.NoError(t, os.Remove(path))
	remaining, err := reconcile(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, counts{Parsed: 1, Skipped: 1, Failed: 1, Messages: 1, MalformedLines: 1}, remaining)
}

func TestRunDoesNotPrintPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	var output, diagnostics bytes.Buffer
	assert.Equal(t, 2, run(t.Context(), root, &output, &diagnostics))
	assert.Empty(t, output.String())
	assert.NotContains(t, diagnostics.String(), root)
}

func TestRunIgnoresNonTranscriptJSONL(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agent-10000000-0000-4000-8000-000000000001", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.jsonl"), []byte("{}\n"), 0o600))

	var output, diagnostics bytes.Buffer
	assert.Equal(t, 2, run(t.Context(), root, &output, &diagnostics))
	assert.JSONEq(t, `{"parsed":0,"skipped":0,"failed":0,"messages":0,"malformed_lines":0}`, output.String())
	assert.Contains(t, diagnostics.String(), "No Muse transcript files were found")

	writeTranscript(t, root, 1, 2, "")
	output.Reset()
	diagnostics.Reset()
	assert.Equal(t, 0, run(t.Context(), root, &output, &diagnostics))
	assert.JSONEq(t, `{"parsed":0,"skipped":1,"failed":0,"messages":0,"malformed_lines":0}`, output.String())
	assert.Empty(t, diagnostics.String())
}

func writeTranscript(t *testing.T, root string, n, version int, records string) string {
	t.Helper()
	id := fmt.Sprintf("10000000-0000-4000-8000-%012d", n)
	dir := filepath.Join(root, "agent-"+id, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	header := fmt.Sprintf(`{"type":"session_header","version":%d,"session_id":%q,"agent_id":%q,"created_at":"2026-09-22T20:27:51.498279614+00:00"}`+"\n", version, id, id)
	path := filepath.Join(dir, id+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(header+records), 0o600))
	return path
}

func message(seq int) string {
	return fmt.Sprintf(`{"type":"item","seq":%d,"source":"runtime","created_at":"2026-09-22T20:27:51.498279614+00:00","item":{"type":"message","role":"assistant","text":"example"}}`+"\n", seq)
}
