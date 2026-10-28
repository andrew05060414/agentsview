package mask

import (
	"io"
	"os"
	"strings"
)

// FileScan is the result of ScanFile: counts per value fingerprint.
type FileScan struct {
	Bytes int64
	// Full counts occurrences of a whole value; Frag counts occurrences of an
	// alphanumeric run (12+ chars) of a value, which is what a full-text index
	// keeps after tokenizing. Both are case-insensitive.
	Full map[string]int
	Frag map[string]int
	// Rule counts rule-pattern hits on the raw bytes, per rule id (case-sensitive,
	// best effort: a value split across database pages is not seen).
	Rule map[string]int
	// FullPages / FragPages count hits per database page (offset / PageSize);
	// they let the caller name the table or index that still holds a copy.
	FullPages map[int64]int
	FragPages map[int64]int
}

// PageSize is the SQLite page size used to bucket hit offsets.
var PageSize int64 = 4096

// ScanFile streams a file (for example a SQLite database after the backfill)
// and reports how often the given secret values, or fragments of them, still
// occur in its raw bytes. It exists to prove that no plaintext copy survives in
// free pages, index segments or the WAL. Only counts are returned.
func ScanFile(path string, values []string, rules *Masker) (*FileScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fs := NewFragmentScanner(values)
	overlap := fs.maxLen + 16
	res := &FileScan{Full: map[string]int{}, Frag: map[string]int{}, Rule: map[string]int{},
		FullPages: map[int64]int{}, FragPages: map[int64]int{}}

	const chunk = 8 << 20
	buf := make([]byte, 0, chunk+overlap)
	carry := 0
	var base int64 // file offset of buf[0]
	for {
		n := chunk
		buf = buf[:carry+n]
		read, rerr := io.ReadFull(f, buf[carry:carry+n])
		buf = buf[:carry+read]
		res.Bytes += int64(read)
		if len(buf) > 0 {
			for _, h := range fs.Find(string(buf)) {
				// A match that lies wholly inside the carried-over region was
				// counted in the previous round.
				if h.End <= carry {
					continue
				}
				page := (base + int64(h.Start)) / PageSize
				if h.Full {
					res.Full[h.FP]++
					res.FullPages[page]++
				} else {
					res.Frag[h.FP]++
					res.FragPages[page]++
				}
			}
			if rules != nil {
				_, hits := rules.Mask(string(buf))
				for _, h := range hits {
					if h.Excluded == "" {
						res.Rule[h.Rule]++
					}
				}
			}
		}
		if rerr != nil {
			break
		}
		newCarry := min(overlap, len(buf))
		base += int64(len(buf) - newCarry)
		carry = newCarry
		copy(buf, buf[len(buf)-carry:])
		buf = buf[:carry]
	}
	return res, nil
}

// FragmentScanner finds whole secret values and 12+ character alphanumeric runs
// of them (what a full-text index keeps after tokenizing) in text,
// case-insensitively.
type FragmentScanner struct {
	vs     *valueSet
	pats   []string
	owner  []string // fingerprint of the value a pattern belongs to
	isFrag []bool
	maxLen int
}

// FragHit is one occurrence found by FragmentScanner.Find.
type FragHit struct {
	FP         string
	Full       bool
	Start, End int
}

// NewFragmentScanner builds a scanner for the given plaintext values.
func NewFragmentScanner(values []string) *FragmentScanner {
	fs := &FragmentScanner{maxLen: 64}
	seen := map[string]bool{}
	for _, v := range values {
		lv := strings.ToLower(v)
		fp := FP8(v)
		if !seen["F"+lv] {
			seen["F"+lv] = true
			fs.pats, fs.owner, fs.isFrag = append(fs.pats, lv), append(fs.owner, fp), append(fs.isFrag, false)
		}
		fs.maxLen = max(fs.maxLen, len(lv))
		for _, run := range alnumRuns(lv, 12) {
			if !seen["R"+run] {
				seen["R"+run] = true
				fs.pats, fs.owner, fs.isFrag = append(fs.pats, run), append(fs.owner, fp), append(fs.isFrag, true)
			}
		}
	}
	fs.vs = newValueSet(fs.pats)
	return fs
}

// Find returns the occurrences in text (ASCII case-folding preserves offsets).
func (fs *FragmentScanner) Find(text string) []FragHit {
	if fs == nil || fs.vs == nil || len(fs.pats) == 0 {
		return nil
	}
	lower := []byte(text)
	for i, c := range lower {
		if c >= 'A' && c <= 'Z' {
			lower[i] = c + 32
		}
	}
	var out []FragHit
	for _, m := range fs.vs.find(string(lower)) {
		out = append(out, FragHit{FP: fs.owner[m.idx], Full: !fs.isFrag[m.idx], Start: m.start, End: m.end})
	}
	return out
}

func alnumRuns(s string, minLen int) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		alnum := i < len(s) && ((s[i] >= '0' && s[i] <= '9') || (s[i] >= 'a' && s[i] <= 'z'))
		if alnum && start < 0 {
			start = i
		}
		if !alnum && start >= 0 {
			if i-start >= minLen {
				out = append(out, s[start:i])
			}
			start = -1
		}
	}
	return out
}
