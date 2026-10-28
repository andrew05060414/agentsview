package mask

import (
	"regexp"
	"strings"
)

// Value shapes. The L1 gate masks a credential-named assignment only when the
// value looks like a random string, not an identifier, path, UUID, number or
// plain hash. The classification is the one used by the 2026-10-27 history
// scan (tools/scan/main.go kind2), so dry-run counts stay comparable.
const (
	shapeUUID    = "uuid"
	shapePath    = "path"
	shapeIdent   = "ident"
	shapeDigits  = "digits"
	shapeHexHash = "hexhash"
	shapeRandom  = "random" // "mixed" in the scan report
)

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	identRe  = regexp.MustCompile(`^[A-Za-z_.:-]+$`)
	sensKey  = regexp.MustCompile(`(secret|token|passw|pwd|api[-_]?key|apikey|auth[-_]?key|private[-_]?key|access[-_]?key|bearer|cookie|credential|(^|[-_.])key$|^key)`)
	keyTrims = " \t:=\"'"
)

// valueShape classifies a candidate secret value.
func valueShape(v string) string {
	switch {
	case uuidRe.MatchString(v):
		return shapeUUID
	case strings.ContainsAny(v, "/\\") || strings.HasPrefix(strings.ToLower(v), "http"):
		return shapePath
	case identRe.MatchString(v):
		return shapeIdent
	}
	allDigit, allHex := true, true
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			allDigit = false
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			allHex = false
		}
	}
	if allDigit {
		return shapeDigits
	}
	if allHex && (len(v) == 32 || len(v) == 40 || len(v) == 64 || len(v) == 128) {
		return shapeHexHash
	}
	return shapeRandom
}

// credentialKeyName reports whether the variable name before an assignment
// looks like it holds a credential.
func credentialKeyName(label string) (name string, ok bool) {
	name = strings.ToLower(strings.Trim(label, keyTrims))
	return name, sensKey.MatchString(name) && !strings.Contains(name, "public")
}
