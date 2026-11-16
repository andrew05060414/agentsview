package mask

import (
	"fmt"
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
	v = strings.TrimRight(v, "=") // base64 padding
	switch {
	case uuidRe.MatchString(v):
		return shapeUUID
	case (strings.ContainsAny(v, "/\\") || strings.HasPrefix(strings.ToLower(v), "http")) && !looksLikeBase64(v):
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

// ValueClass describes a value's character make-up for reports without
// revealing it: the classes present (d digit, l lower, U upper, s symbol) and
// the Shannon entropy in bits per byte.
func ValueClass(v string) (class string, entropy float64) {
	var dig, lo, up, sym bool
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9':
			dig = true
		case c >= 'a' && c <= 'z':
			lo = true
		case c >= 'A' && c <= 'Z':
			up = true
		default:
			sym = true
		}
	}
	if dig {
		class += "d"
	}
	if lo {
		class += "l"
	}
	if up {
		class += "U"
	}
	if sym {
		class += "s"
	}
	return class, shannon(v)
}

// NewLiteralMasker returns a Masker that only replaces the given literal
// values everywhere (no pattern rules). Used to measure how common candidate
// closure values are before they are admitted.
func NewLiteralMasker(rs *RuleSet, values []string) (*Masker, error) {
	lit := *rs
	lit.Rules = nil
	return New(&lit, Options{Closure: values})
}

// jsonEscapeInner returns v as it appears inside a JSON string literal (the
// characters JSON must escape), without the surrounding quotes.
func jsonEscapeInner(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(fmt.Sprintf(`\u%04x`, c))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// looksLikeBase64 separates a random base64 string (which may contain '/')
// from a file path or URL: long, mixed case with digits, high entropy, few
// slashes, and not rooted or schemed like a path.
func looksLikeBase64(v string) bool {
	if len(v) < 24 || strings.Contains(v, "://") || strings.HasPrefix(strings.ToLower(v), "http") {
		return false
	}
	if c := v[0]; c == '/' || c == '~' || c == '.' || c == '\\' {
		return false
	}
	var up, lo, dig bool
	slashes := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= 'A' && c <= 'Z':
			up = true
		case c >= 'a' && c <= 'z':
			lo = true
		case c >= '0' && c <= '9':
			dig = true
		case c == '/' || c == '\\':
			slashes++
		}
	}
	return up && lo && dig && slashes <= len(v)/6 && shannon(v) >= 4.0
}

// slugLike reports a hyphen/underscore/dot separated name made of short words
// (a file stem, a branch or plan name, a dated slug). Such strings pass the
// "random" shape test but are identifiers; promoting one to the value closure
// would rewrite every file name and path that contains it.
func slugLike(v string) bool {
	segs := strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if len(segs) < 3 {
		return false
	}
	for _, s := range segs {
		if len(s) > 8 {
			return false
		}
	}
	return true
}
