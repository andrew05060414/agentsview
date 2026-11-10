package stringutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeUTF8 strips NUL bytes, replaces invalid UTF-8 sequences,
// and removes control runes (other than \n, \t, \r).
//
// The control-rune strip runs per rune, not per byte: C1 controls
// (U+0080..U+009F) are valid two-byte UTF-8 and survive
// strings.ToValidUTF8, so a terminal escape such as ESC ]0;...BEL
// would otherwise persist intact. A raw NUL must be removed before
// strings.ToValidUTF8, which treats it as valid. Callers compare
// sanitized values across the write path and PostgreSQL readback, so
// this MUST stay idempotent: SanitizeUTF8(SanitizeUTF8(s)) ==
// SanitizeUTF8(s).
func SanitizeUTF8(s string) string {
	if isCleanText(s) {
		return s
	}
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	// Fast path: skip the rune scan and allocation when the string
	// carries no control runes to strip.
	if strings.IndexFunc(s, isStrippableControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isStrippableControl(r) {
			return -1
		}
		return r
	}, s)
}

// isCleanText reports whether SanitizeUTF8 would return s unchanged:
// valid UTF-8 with no NUL or strippable control rune. Most transcript
// text needs no repair, so one pass that skips ASCII decoding avoids
// the separate NUL, UTF-8 and control scans of the repair path.
func isCleanText(s string) bool {
	const (
		spaces = 0x2020202020202020
		ones   = 0x0101010101010101
		highs  = 0x8080808080808080
	)
	for i := 0; i < len(s); {
		if len(s)-i >= 8 {
			// Skip 8 bytes of printable ASCII (0x20..0x7e) at once.
			// Subtracting spaces sets the high bit of a byte below 0x20
			// or at 0xa0 and above; adding ones sets it for DEL and
			// 0x80..0xfe. A borrow or carry only starts at a byte that
			// is already flagged, so a clean block always yields zero; a
			// false positive only sends bytes to the check below. Keep
			// this inline: the compiler does not inline it as a separate
			// function, which adds a call for every non-ASCII rune.
			b := s[i : i+8]
			word := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 |
				uint64(b[3])<<24 | uint64(b[4])<<32 | uint64(b[5])<<40 |
				uint64(b[6])<<48 | uint64(b[7])<<56
			if ((word-spaces)|(word+ones))&highs == 0 {
				i += 8
				continue
			}
		}
		c := s[i]
		if c < utf8.RuneSelf {
			if c == 0x7f || c < 0x20 && c != '\n' && c != '\t' && c != '\r' {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || isStrippableControl(r) {
			return false
		}
		i += size
	}
	return true
}

// isStrippableControl reports whether r is a control rune that
// SanitizeUTF8 removes. Newline, tab, and carriage return are
// preserved because they are legitimate whitespace in message
// content; every other control rune (C0 below U+0020, DEL, and the
// C1 block U+0080..U+009F) is stripped.
func isStrippableControl(r rune) bool {
	if r == '\n' || r == '\t' || r == '\r' {
		return false
	}
	return unicode.IsControl(r)
}
