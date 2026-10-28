// Package mask replaces secrets in stored conversation text with
// deterministic one-way placeholders of the form {{SECRET_xxxxxxxx}}.
//
// The placeholder is the first 8 hex characters of sha256(value), the same
// identity OpenCodex's realtime request redaction uses, so one value has the
// same placeholder in a live request and in stored history. No mapping is kept
// and there is no restore path.
//
// Detection (tier L1): vendor-format keys, JWTs, passwords inside connection
// strings, private-key blocks, and values that look random under a
// credential-like name (rules.v1.json), plus literal values: the known secrets
// of the local secrets directory and every value those rules confirmed
// elsewhere (the "value closure"). Mask is a pure, deterministic and
// idempotent function of its input and the Masker's configuration.
package mask

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Hit describes one masked (or, when Excluded, deliberately kept) span. It never
// exposes the value through formatting; only the rule, the 8-hex fingerprint and
// the length. The plaintext is reachable through ClosureValue for the one-shot
// backfill, which keeps it in memory only.
type Hit struct {
	Rule     string
	FP       string
	Len      int
	Excluded string // non-empty: the span matched a rule but a gate kept it (reason)
	closure  string // value eligible for the value closure ("" if not)
}

// String implements fmt.Stringer so a Hit that is printed by mistake stays safe.
func (h Hit) String() string { return fmt.Sprintf("hit{%s %s len=%d}", h.Rule, h.FP, h.Len) }

// GoString keeps %#v from printing the closure value.
func (h Hit) GoString() string { return h.String() }

// ClosureValue returns the plaintext to add to the value closure, if the span
// qualifies. Callers must never log it.
func (h Hit) ClosureValue() (string, bool) { return h.closure, h.closure != "" }

// Options configures a Masker.
type Options struct {
	// Exact are literal secrets to replace wherever they occur (plaintext, in
	// memory only). Label is kept for reports.
	Exact []ExactValue
	// Closure are literal values confirmed as secrets elsewhere in the data.
	Closure []string
	// Registry holds hashed known values for ingestion-time matching.
	Registry *Registry
	// Allow lists values that must never be masked.
	Allow map[string]struct{}
}

// Masker applies the rule set. It is immutable and safe for concurrent use.
type Masker struct {
	rules      []*compiledRule
	closureMin int
	closureEnt float64
	vs         *valueSet
	vsLabel    []string // per value: label ("" for closure values)
	vsCanon    []string // per value: the value whose placeholder it takes (differs for escaped variants)
	reg        *Registry
	allow      map[string]struct{}
}

// New builds a Masker from a rule set.
func New(rs *RuleSet, o Options) (*Masker, error) {
	rules, err := rs.compile()
	if err != nil {
		return nil, err
	}
	m := &Masker{
		rules: rules, closureMin: rs.Closure.RandomMinLen, closureEnt: rs.Closure.RandomMinEntropy,
		reg: o.Registry, allow: o.Allow,
	}
	type lv struct{ v, label, canon string }
	var lits []lv
	seen := map[string]bool{}
	for _, e := range o.Exact {
		if len(e.Value) >= exactMinLen && !seen[e.Value] {
			seen[e.Value] = true
			lits = append(lits, lv{e.Value, e.Label, e.Value})
		}
		// The same value inside a JSON string (tool inputs, state blobs) is
		// escaped; it gets the original value's placeholder.
		if esc := jsonEscapeInner(e.Value); esc != e.Value && len(esc) >= exactMinLen && !seen[esc] {
			seen[esc] = true
			lits = append(lits, lv{esc, e.Label, e.Value})
		}
	}
	for _, v := range o.Closure {
		if len(v) >= exactMinLen && !seen[v] {
			seen[v] = true
			lits = append(lits, lv{v, "", v})
		}
	}
	if len(lits) > 0 {
		sort.SliceStable(lits, func(i, j int) bool { return len(lits[i].v) > len(lits[j].v) })
		vals := make([]string, len(lits))
		m.vsLabel = make([]string, len(lits))
		m.vsCanon = make([]string, len(lits))
		for i, l := range lits {
			vals[i], m.vsLabel[i], m.vsCanon[i] = l.v, l.label, l.canon
		}
		m.vs = newValueSet(vals)
	}
	return m, nil
}

// NewDefault builds a Masker from the embedded rules.
func NewDefault(o Options) (*Masker, error) {
	rs, err := LoadRules()
	if err != nil {
		return nil, err
	}
	return New(rs, o)
}

// Text masks text and discards the hit details.
func (m *Masker) Text(text string) string {
	out, _ := m.Mask(text)
	return out
}

// Mask returns the masked text and the hits. If nothing matched, the input is
// returned unchanged. Order: literal values, registry hashes, then rules in
// file order (the order OpenCodex applies them), so a value that contains
// characters a pattern would cut short is replaced as a whole.
func (m *Masker) Mask(text string) (string, []Hit) {
	if text == "" {
		return text, nil
	}
	var hits []Hit
	if m.vs != nil {
		text, hits = m.replaceLiterals(text, hits)
	}
	if m.reg != nil {
		text, hits = m.reg.mask(text, hits)
	}
	var lower string
	haveLower := false
	for _, r := range m.rules {
		if len(r.spec.Prefilter) > 0 {
			hay := text
			if r.spec.PrefilterFold {
				if !haveLower {
					lower, haveLower = strings.ToLower(text), true
				}
				hay = lower
			}
			if !containsAny(hay, r.spec.Prefilter) {
				continue
			}
		}
		var changed bool
		if r.anchored != nil && !haveLower {
			lower, haveLower = strings.ToLower(text), true
		}
		text, hits, changed = m.applyRule(r, text, lower, hits)
		if changed {
			haveLower = false
		}
	}
	return text, hits
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (m *Masker) replaceLiterals(text string, hits []Hit) (string, []Hit) {
	found := m.vs.find(text)
	if len(found) == 0 {
		return text, hits
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, f := range found {
		val := m.vsCanon[f.idx]
		if m.allowed(val) {
			continue
		}
		b.WriteString(text[last:f.start])
		b.WriteString(Placeholder(val))
		last = f.end
		rule := "closure"
		if lbl := m.vsLabel[f.idx]; lbl != "" {
			rule = "exact:" + lbl
		}
		hits = append(hits, Hit{Rule: rule, FP: FP8(val), Len: len(val)})
	}
	b.WriteString(text[last:])
	return b.String(), hits
}

func (m *Masker) allowed(v string) bool {
	if m.allow == nil {
		return false
	}
	_, ok := m.allow[v]
	return ok
}

// applyRule mirrors OpenCodex's applyRule: collect the matches of the pattern
// on the current text, then for each one replace every occurrence of the full
// matched text by prefix + placeholder + suffix.
func (m *Masker) applyRule(r *compiledRule, text, lower string, hits []Hit) (string, []Hit, bool) {
	var locs [][]int
	if r.anchored != nil && !disableAnchoredForTest {
		locs = findAnchored(r, text, lower)
	} else {
		locs = r.re.FindAllStringSubmatchIndex(text, -1)
	}
	if len(locs) == 0 {
		return text, hits, false
	}
	type repl struct{ full, replacement string }
	var repls []repl
	for _, loc := range locs {
		ss, se := -1, -1
		for _, g := range r.secrets {
			if loc[2*g] >= 0 {
				ss, se = loc[2*g], loc[2*g+1]
				break
			}
		}
		if ss < 0 || se <= ss {
			continue
		}
		secret := text[ss:se]
		if IsPlaceholder(secret) || m.allowed(secret) {
			continue
		}
		hit := Hit{Rule: r.spec.ID, FP: FP8(secret), Len: len(secret)}
		if g := r.spec.Gate; g != nil {
			if reason := m.gate(g, text, loc, secret); reason != "" {
				hit.Excluded = reason
				hits = append(hits, hit)
				continue
			}
		}
		if m.closureEligible(r, secret) {
			hit.closure = secret
		}
		hits = append(hits, hit)
		full := text[loc[0]:loc[1]]
		repls = append(repls, repl{full, text[loc[0]:ss] + Placeholder(secret) + text[se:loc[1]]})
	}
	if len(repls) == 0 {
		return text, hits, false
	}
	out := text
	for _, rp := range repls {
		out = strings.ReplaceAll(out, rp.full, rp.replacement)
	}
	return out, hits, out != text
}

// gate returns "" when the span passes the gate, otherwise the reason it is kept.
func (m *Masker) gate(g *GateSpec, text string, loc []int, secret string) string {
	if g.KeyName == "credential" {
		ks, ke := loc[2*g.KeyGroup], loc[2*g.KeyGroup+1]
		if ks < 0 {
			return "no-key"
		}
		if _, ok := credentialKeyName(text[ks:ke]); !ok {
			return "key-not-credential"
		}
	}
	if len(g.ValueShapes) > 0 {
		s := valueShape(secret)
		if !slices.Contains(g.ValueShapes, s) {
			return "shape-" + s
		}
	}
	return ""
}

func (m *Masker) closureEligible(r *compiledRule, secret string) bool {
	switch r.spec.Closure {
	case "any":
		return len(secret) >= exactMinLen
	case "random":
		return len(secret) >= m.closureMin && valueShape(secret) == shapeRandom &&
			shannon(secret) >= m.closureEnt && !slugLike(secret)
	}
	return false
}

// disableAnchoredForTest makes AnchorAtKeywords rules scan the whole text, so a
// test can prove both strategies find the same matches.
var disableAnchoredForTest bool

func isIdentByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// findAnchored returns the matches r.re.FindAllStringSubmatchIndex would return,
// found by trying the pattern only where an identifier contains a keyword. Every
// match of such a pattern starts at the beginning of the identifier run (or at a
// quote right before it) that holds the keyword, so no other position can start
// one. lower is strings.ToLower(text) (ASCII keywords keep byte offsets).
func findAnchored(r *compiledRule, text, lower string) [][]int {
	var pos []int
	for _, kw := range r.spec.Prefilter {
		for from := 0; from < len(lower); {
			i := strings.Index(lower[from:], kw)
			if i < 0 {
				break
			}
			pos = append(pos, from+i)
			from += i + 1
		}
	}
	if len(pos) == 0 {
		return nil
	}
	sort.Ints(pos)
	var out [][]int
	next, lastStart := 0, -1
	for _, p := range pos {
		if p < next {
			continue // inside the previous match
		}
		s := p
		for s > next && isIdentByte(text[s-1]) {
			s--
		}
		if s > next && (text[s-1] == '"' || text[s-1] == '\'') {
			s--
		}
		if s == lastStart {
			continue // another keyword of an identifier that was already tried
		}
		lastStart = s
		loc := r.anchored.FindStringSubmatchIndex(text[s:])
		if loc == nil {
			continue
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += s
			}
		}
		out = append(out, loc)
		next = loc[1]
	}
	return out
}
