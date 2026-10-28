package sync

import (
	"os"
	"testing"

	"go.kenn.io/agentsview/internal/mask"
)

// Fixtures in this package store plaintext secrets on purpose to exercise
// scanning and recompute paths; tests of ingestion-time masking switch it on
// explicitly with mask.SetDefaultForTest.
func TestMain(m *testing.M) {
	_ = os.Setenv(mask.EnvMask, "off")
	os.Exit(m.Run())
}
