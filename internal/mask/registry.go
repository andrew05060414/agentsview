package mask

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Registry is a set of known secret values kept only as sha256 hashes, so it
// can be copied between machines without being a copy of the secrets. A
// stream of ingested text is split into tokens and each token whose length
// matches some registered length is hashed and looked up. It catches bare
// occurrences of a known value that no pattern would flag. Values that contain
// token delimiters (spaces, quotes, separators) cannot be found this way; the
// one-shot backfill covers those with plaintext matching.
type Registry struct {
	byLen map[int]map[[32]byte]string // length -> hash -> label
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byLen: map[int]map[[32]byte]string{}} }

// Len returns the number of registered hashes.
func (r *Registry) Len() int {
	n := 0
	for _, m := range r.byLen {
		n += len(m)
	}
	return n
}

// Add registers a value (only its hash and length are kept).
func (r *Registry) Add(label, value string) {
	if len(value) < minRegistryLen {
		return
	}
	m := r.byLen[len(value)]
	if m == nil {
		m = map[[32]byte]string{}
		r.byLen[len(value)] = m
	}
	m[sha256.Sum256([]byte(value))] = label
}

const minRegistryLen = 8

type registryLine struct {
	SHA256 string `json:"sha256"`
	Len    int    `json:"len"`
	Label  string `json:"label"`
}

// LoadRegistry reads a JSONL registry file written by Save.
func LoadRegistry(path string) (*Registry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := NewRegistry()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rl registryLine
		if err := json.Unmarshal([]byte(line), &rl); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, ln, err)
		}
		raw, err := hex.DecodeString(rl.SHA256)
		if err != nil || len(raw) != 32 || rl.Len < minRegistryLen {
			return nil, fmt.Errorf("%s:%d: malformed registry line", path, ln)
		}
		var h [32]byte
		copy(h[:], raw)
		m := r.byLen[rl.Len]
		if m == nil {
			m = map[[32]byte]string{}
			r.byLen[rl.Len] = m
		}
		m[h] = rl.Label
	}
	return r, sc.Err()
}

// Save writes the registry as sorted JSONL (hash, length, label; never a value).
func (r *Registry) Save(path string) error {
	var lines []registryLine
	for l, m := range r.byLen {
		for h, label := range m {
			lines = append(lines, registryLine{SHA256: hex.EncodeToString(h[:]), Len: l, Label: label})
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].SHA256 < lines[j].SHA256 })
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Merge adds every entry of other.
func (r *Registry) Merge(other *Registry) {
	for l, m := range other.byLen {
		dst := r.byLen[l]
		if dst == nil {
			dst = map[[32]byte]string{}
			r.byLen[l] = dst
		}
		for h, label := range m {
			if _, ok := dst[h]; !ok {
				dst[h] = label
			}
		}
	}
}

func isTokenDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\v', '\f', '"', '\'', '`', ',', ';', '(', ')', '[', ']', '{', '}', '<', '>', '|', '\\':
		return true
	}
	return false
}

// subDelims splits a token further so `NAME=value`, `a:b`, URL paths and query
// strings expose the value as its own candidate.
func isSubDelim(c byte) bool {
	switch c {
	case '=', ':', '&', '?', '/', '#', '@':
		return true
	}
	return false
}

// mask replaces registered values in text. Hits carry Rule "registry".
func (r *Registry) mask(text string, hits []Hit) (string, []Hit) {
	if r == nil || len(r.byLen) == 0 {
		return text, hits
	}
	type rep struct{ s, e int }
	var reps []rep
	check := func(s, e int) bool {
		m := r.byLen[e-s]
		if m == nil {
			return false
		}
		cand := text[s:e]
		if IsPlaceholder(cand) {
			return false
		}
		h := sha256.Sum256([]byte(cand))
		label, ok := m[h]
		if !ok {
			return false
		}
		reps = append(reps, rep{s, e})
		hits = append(hits, Hit{Rule: "registry:" + label, FP: FP8(cand), Len: e - s})
		return true
	}
	i := 0
	for i < len(text) {
		for i < len(text) && isTokenDelim(text[i]) {
			i++
		}
		start := i
		for i < len(text) && !isTokenDelim(text[i]) {
			i++
		}
		if i == start {
			continue
		}
		if check(start, i) {
			continue
		}
		// Try the pieces between sub-delimiters.
		ps := start
		for j := start; j <= i; j++ {
			if j == i || isSubDelim(text[j]) {
				if j > ps {
					check(ps, j)
				}
				ps = j + 1
			}
		}
	}
	if len(reps) == 0 {
		return text, hits
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, rp := range reps {
		if rp.s < last {
			continue
		}
		b.WriteString(text[last:rp.s])
		b.WriteString(Placeholder(text[rp.s:rp.e]))
		last = rp.e
	}
	b.WriteString(text[last:])
	return b.String(), hits
}
