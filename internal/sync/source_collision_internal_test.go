package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Remote imports store host:/path source paths. Only a local mirror file that
// is missing proves the owner gone; an unmapped path still owns its id.
func TestSourceFileElsewhereResolvesRewrittenPaths(t *testing.T) {
	mirror := filepath.Join(t.TempDir(), "owner.json")
	require.NoError(t, os.WriteFile(mirror, []byte("{}"), 0o644))
	e := &Engine{pathRewriter: func(p string) string { return "host:" + p }}
	const owner, incoming = "host:/remote/owner.json", "host:/remote/other.json"

	assert.True(t, e.sourceFileElsewhere(owner, incoming), "no resolver")
	e.storedPathResolver = func(p string) (string, bool) { return "", false }
	assert.True(t, e.sourceFileElsewhere(owner, incoming), "no mirror")
	e.storedPathResolver = func(p string) (string, bool) { return mirror, p == owner }
	assert.True(t, e.sourceFileElsewhere(owner, incoming), "mirror present")
	require.NoError(t, os.Remove(mirror))
	assert.False(t, e.sourceFileElsewhere(owner, incoming), "mirror gone")
}

// Two paths that name one file are the same source, not a collision.
func TestSourceFileElsewhereIgnoresSameFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	e := &Engine{}
	assert.False(t, e.sourceFileElsewhere(filepath.Join(dir, ".", "session.json"), path))
}
