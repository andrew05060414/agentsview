package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// runAnnotateCommand executes cmd with args and returns its error. Flag and
// argument validation runs before any service resolution, so these cases
// never touch a store.
func runAnnotateCommand(
	t *testing.T, cmd *cobra.Command, args ...string,
) error {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func TestSessionLabelRejectsConflictingFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			"clear combined with a label",
			[]string{"sess-1", "--clear", "role=x"},
			"--clear cannot be combined",
		},
		{
			"replace combined with remove",
			[]string{"sess-1", "--replace", "role=x", "--remove", "role=y"},
			"--replace cannot be combined with --remove",
		},
		{
			"nothing to do",
			[]string{"sess-1"},
			"give labels to add, --remove, --replace, or --clear",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAnnotateCommand(t, newSessionLabelCommand(), tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestSessionParentRequiresParentOrClear(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			"neither parent nor clear",
			[]string{"child-1"},
			"give either a parent session id or --clear",
		},
		{
			"parent and clear together",
			[]string{"child-1", "parent-1", "--clear"},
			"give either a parent session id or --clear",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAnnotateCommand(t, newSessionParentCommand(), tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestPrintSessionLabelsHuman pins the terminal format a user sees,
// including the pending-sync note and control-byte sanitization.
func TestPrintSessionLabelsHuman(t *testing.T) {
	var buf bytes.Buffer
	printSessionLabelsHuman(&buf, &db.SessionLabels{
		SessionID: "sess-1", Labels: []string{"ticket=1", "role=reviewer"},
		SessionFound: true,
	})
	assert.Equal(t, "sess-1: ticket=1, role=reviewer\n", buf.String())

	buf.Reset()
	printSessionLabelsHuman(&buf, &db.SessionLabels{
		SessionID: "sess-2", SessionFound: true,
	})
	assert.Equal(t, "sess-2: (no labels)\n", buf.String())

	buf.Reset()
	printSessionLabelsHuman(&buf, &db.SessionLabels{
		SessionID: "sess-3", Labels: []string{"a"}, SessionFound: false,
	})
	out := buf.String()
	assert.Contains(t, out, "sess-3: a")
	assert.Contains(t, out, "labels apply once it is")

	// Terminal control bytes in a session id must not reach the terminal.
	buf.Reset()
	printSessionLabelsHuman(&buf, &db.SessionLabels{
		SessionID:    "sess\x1b]8;;http://evil\x07-4",
		Labels:       []string{"a"},
		SessionFound: true,
	})
	assert.NotContains(t, buf.String(), "\x1b]8")
}

func TestPrintSessionParentHuman(t *testing.T) {
	var buf bytes.Buffer
	printSessionParentHuman(&buf, "sess-1", &db.SessionExternalParent{
		SessionID: "sess-1", ParentSessionID: "mgr-1",
		RelationshipType: "subagent", SessionFound: true, Applied: true,
	}, false)
	assert.Equal(t, "sess-1: parent mgr-1 (subagent)\n", buf.String())

	buf.Reset()
	printSessionParentHuman(&buf, "sess-1", &db.SessionExternalParent{
		SessionID: "sess-1", SessionFound: true,
	}, true)
	assert.Equal(t, "sess-1: no launcher parent to remove\n", buf.String())

	buf.Reset()
	printSessionParentHuman(&buf, "sess-1", &db.SessionExternalParent{
		SessionID: "sess-1", ParentSessionID: "mgr-1", SessionFound: true,
	}, true)
	assert.Equal(t, "sess-1: removed parent mgr-1\n", buf.String())

	buf.Reset()
	printSessionParentHuman(&buf, "sess-1", &db.SessionExternalParent{
		SessionID: "sess-1", ParentSessionID: "mgr-1",
		RelationshipType: "subagent",
	}, false)
	assert.Contains(t, buf.String(), "applies after both sessions sync")

	buf.Reset()
	printSessionParentHuman(&buf, "sess-1", &db.SessionExternalParent{
		SessionID: "sess-1", ParentSessionID: "mgr-1",
		RelationshipType: "subagent", SessionFound: true, Applied: false,
	}, false)
	assert.Contains(t, buf.String(), "link stored but not applied")
}
