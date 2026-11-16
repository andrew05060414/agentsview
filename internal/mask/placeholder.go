package mask

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Placeholder shape shared with OpenCodex's realtime redaction
// (src/privacy/redact.ts): {{SECRET_<first 8 hex of sha256(value)>}}. The same
// value always yields the same placeholder, so no mapping table is needed and
// there is no restore path.
const (
	PlaceholderPrefix = "{{SECRET_"
	PlaceholderSuffix = "}}"
	fpHexChars        = 8
)

var placeholderRe = regexp.MustCompile(`\{\{SECRET_[0-9a-fA-F]{8}\}\}`)

// FP8 returns the first 8 hex characters of sha256(value). It is the only
// identity of a masked value that may appear in logs and reports.
func FP8(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:fpHexChars]
}

// Placeholder returns the placeholder text that replaces value.
func Placeholder(value string) string {
	return PlaceholderPrefix + FP8(value) + PlaceholderSuffix
}

// IsPlaceholder mirrors OpenCodex's isAlreadyPlaceholder: the value starts and
// ends with the placeholder delimiters and contains a well-formed placeholder.
// Masking such a span again is a no-op, which keeps Mask idempotent.
func IsPlaceholder(value string) bool {
	return strings.HasPrefix(value, PlaceholderPrefix) &&
		strings.HasSuffix(value, PlaceholderSuffix) &&
		placeholderRe.MatchString(value)
}

// CountPlaceholders returns how many well-formed placeholders text contains.
func CountPlaceholders(text string) int {
	if !strings.Contains(text, PlaceholderPrefix) {
		return 0
	}
	return len(placeholderRe.FindAllStringIndex(text, -1))
}
