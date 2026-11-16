//go:build windows

package parser

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime returns when the file was created, if the platform records it.
func fileBirthTime(info os.FileInfo) (time.Time, bool) {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attrs == nil {
		return time.Time{}, false
	}
	created := attrs.CreationTime.Nanoseconds()
	if created <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, created), true
}
