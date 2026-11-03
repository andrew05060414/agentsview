package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/mask"
	"go.kenn.io/agentsview/internal/secrets"
)

// Fake secrets are assembled at run time so no complete key sits in the source.
func fakeAnthropicKey() string {
	return "sk-" + "ant-" + "api03-" + "Zx9Qw8Er7" + "Ty6Ui5Op4" + "As3Df2Gh1Jk0"
}

func secretScanFixture(secret string) (db.Session, []db.Message) {
	session := db.Session{ID: "s1", Project: "p", Machine: "local", Agent: "claude"}
	msgs := []db.Message{{
		SessionID: "s1", Ordinal: 0, Role: "user",
		Content: "my key is " + secret + " please check",
		ToolCalls: []db.ToolCall{{
			ToolName: "Bash", Category: "Bash",
			InputJSON:     `{"command":"echo ` + secret + `"}`,
			ResultContent: "got " + secret,
		}},
	}}
	return session, msgs
}

// Findings describe the stored (masked) text: a secret the masker replaces is
// not a finding, so the session is not flagged for something that is gone.
func TestScanSecretsFromMessagesIgnoresMaskedSecrets(t *testing.T) {
	m, err := mask.NewDefault(mask.Options{})
	require.NoError(t, err)
	t.Cleanup(mask.SetDefaultForTest(m))

	session, msgs := secretScanFixture(fakeAnthropicKey())
	findings, leak := ScanSecretsFromMessages(session, msgs, secrets.ScanDefinite)
	assert.Empty(t, findings)
	assert.Zero(t, leak)
	assert.Contains(t, msgs[0].Content, fakeAnthropicKey(),
		"scanning must not modify the caller's messages")
}

// With masking off the stored text still holds the secret, so it is flagged,
// at offsets that address the stored content.
func TestScanSecretsFromMessagesFlagsUnmaskedSecrets(t *testing.T) {
	t.Cleanup(mask.SetDefaultForTest(nil))

	session, msgs := secretScanFixture(fakeAnthropicKey())
	findings, leak := ScanSecretsFromMessages(session, msgs, secrets.ScanDefinite)
	require.NotEmpty(t, findings)
	assert.Equal(t, len(findings), leak)
	for _, f := range findings {
		if f.LocationKind == "message" {
			assert.Equal(t, fakeAnthropicKey(), msgs[0].Content[f.MatchStart:f.MatchEnd])
		}
	}
}
