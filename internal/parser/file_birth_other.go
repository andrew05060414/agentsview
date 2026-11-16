//go:build !windows && !darwin

package parser

import (
	"os"
	"time"
)

// fileBirthTime reports no creation time. os.FileInfo does not carry one on
// these platforms, so callers keep their modification-time fallback.
func fileBirthTime(os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
