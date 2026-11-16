//go:build darwin

package parser

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime returns when the file was created, if the platform records it.
func fileBirthTime(info os.FileInfo) (time.Time, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return time.Time{}, false
	}
	birth := stat.Birthtimespec
	if birth.Sec <= 0 && birth.Nsec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(birth.Sec, birth.Nsec), true
}
