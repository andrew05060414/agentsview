package mask

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ExactValue is a literal secret read from the local secrets directory. The
// plaintext lives only in memory. Reports may show Label, length and FP8.
type ExactValue struct {
	Label  string
	Value  string
	Key    string // credential-looking variable name the value was assigned to, if any
	Strict bool   // known real secret: value came from a credential-named assignment, a whole-file secret, or a file holding a single value
}

var (
	exactSensKey = regexp.MustCompile(`(?i)(key|secret|token|pass|pwd|auth|cred|bearer|cookie|session|private)`)
	exactKVRe    = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_.-]*)["']?\s*[=:]\s*["']?([^\s"',;]+)`)
	exactSplitRe = regexp.MustCompile(`[\s"',;\x00-\x1f]+`)
)

const (
	exactMaxFileBytes = 64 * 1024
	exactMinLen       = 8
	exactMaxLen       = 400
)

// LoadExactDir extracts candidate secret values from the top-level files of dir
// (skipping .ps1 scripts, sub-directories, binary files and files over 64 KB).
// The extraction heuristics are those used by the 2026-10-27 history scan so
// that match counts stay comparable. Only Strict values should be used for
// masking; the rest need a human decision.
func LoadExactDir(dir string) ([]ExactValue, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []ExactValue
	seen := map[string]bool{}
	add := func(label, t string, ok bool, key string) {
		t = strings.Trim(t, "{}[]()<>`,;")
		if !ok || !plausibleSecret(t) || seen[label+"|"+t] {
			return
		}
		seen[label+"|"+t] = true
		out = append(out, ExactValue{Label: label, Value: t, Key: key})
	}
	for _, e := range ents {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if e.IsDir() || ext == ".ps1" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil || len(b) == 0 || len(b) > exactMaxFileBytes || bytes.IndexByte(b, 0) >= 0 {
			continue
		}
		label := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		txt := string(b)
		trim := strings.TrimSpace(txt)
		if !strings.ContainsAny(trim, "\r\n") && len(trim) >= 8 && len(trim) <= 200 && !strings.Contains(trim, " ") {
			add(label, trim, true, "")
		}
		for _, m := range exactKVRe.FindAllStringSubmatch(txt, -1) {
			if exactSensKey.MatchString(m[1]) {
				add(label, m[2], true, m[1])
			}
		}
		for _, t := range exactSplitRe.Split(txt, -1) {
			t = strings.TrimLeft(strings.Trim(t, "{}[]()<>`"), ":=")
			if i := strings.Index(t, "="); i > 0 && i < len(t)-1 {
				t = t[i+1:]
			}
			add(label, t, len(t) >= 20 && shannon(t) >= 3.5 && hasDigitOrSym(t), "")
		}
	}
	perLabel := map[string]int{}
	for _, v := range out {
		perLabel[v.Label]++
	}
	for i := range out {
		out[i].Strict = out[i].Key != "" || perLabel[out[i].Label] == 1
	}
	return out, nil
}

func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	var f [256]float64
	for i := 0; i < len(s); i++ {
		f[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range f {
		if c > 0 {
			p := c / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

func hasDigitOrSym(t string) bool {
	for _, r := range t {
		if (r >= '0' && r <= '9') || strings.ContainsRune("!@#$%^&*+/=~?", r) {
			return true
		}
	}
	return false
}

func plausibleSecret(t string) bool {
	if len(t) < exactMinLen || len(t) > exactMaxLen {
		return false
	}
	l := strings.ToLower(t)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "wss://") ||
		strings.ContainsAny(t[:1], `/\.`) || (len(t) > 2 && t[1] == ':' && (t[2] == 92 || t[2] == 47)) {
		return false
	}
	var dig, up, lo, sym int
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			dig++
		case r >= 'A' && r <= 'Z':
			up++
		case r >= 'a' && r <= 'z':
			lo++
		default:
			sym++
		}
	}
	classes := 0
	for _, c := range []int{dig, up, lo, sym} {
		if c > 0 {
			classes++
		}
	}
	if dig == 0 && !strings.ContainsAny(t, "!@#$%^&*+/=~?") {
		return false // identifiers / words / hostnames
	}
	if len(t) < 12 {
		return classes >= 3
	}
	return classes >= 2
}
