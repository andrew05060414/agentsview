package mask

import "sort"

// valueSet finds every occurrence of a fixed set of byte strings in a text with
// one pass (Aho-Corasick with a dense transition table). It backs the exact-value
// and value-closure stages: a value that was recognised as a secret anywhere is
// replaced everywhere, including bare occurrences no pattern would catch.
//
// Matching is leftmost-longest and non-overlapping, like strings.Replacer with
// longest-first arguments.
type valueSet struct {
	delta  []int32 // state*256 + byte -> next state
	outLen []int32 // longest pattern ending at this state (via suffix links); 0 = none
	outIdx []int32 // index of that pattern in values
	values []string
}

func newValueSet(values []string) *valueSet {
	vs := &valueSet{values: values}
	// Build the trie.
	next := [][256]int32{{}}
	vs.outLen = []int32{0}
	vs.outIdx = []int32{-1}
	for i, v := range values {
		if v == "" {
			continue
		}
		s := int32(0)
		for j := 0; j < len(v); j++ {
			c := v[j]
			if next[s][c] == 0 {
				next = append(next, [256]int32{})
				vs.outLen = append(vs.outLen, 0)
				vs.outIdx = append(vs.outIdx, -1)
				next[s][c] = int32(len(next) - 1)
			}
			s = next[s][c]
		}
		if int32(len(v)) > vs.outLen[s] {
			vs.outLen[s] = int32(len(v))
			vs.outIdx[s] = int32(i)
		}
	}
	// BFS to add failure links and complete the transition function.
	n := len(next)
	fail := make([]int32, n)
	queue := make([]int32, 0, n)
	for c := 0; c < 256; c++ {
		if t := next[0][c]; t != 0 {
			fail[t] = 0
			queue = append(queue, t)
		}
	}
	for head := 0; head < len(queue); head++ {
		s := queue[head]
		// Inherit the longest output reachable through the suffix link.
		if f := fail[s]; vs.outLen[f] > vs.outLen[s] {
			vs.outLen[s] = vs.outLen[f]
			vs.outIdx[s] = vs.outIdx[f]
		}
		for c := 0; c < 256; c++ {
			t := next[s][c]
			if t == 0 {
				next[s][c] = next[fail[s]][c]
				continue
			}
			fail[t] = next[fail[s]][c]
			queue = append(queue, t)
		}
	}
	vs.delta = make([]int32, n*256)
	for s := 0; s < n; s++ {
		copy(vs.delta[s*256:(s+1)*256], next[s][:])
	}
	return vs
}

type vsMatch struct{ start, end, idx int }

// find returns the non-overlapping leftmost-longest matches of text.
func (vs *valueSet) find(text string) []vsMatch {
	if vs == nil || len(vs.values) == 0 {
		return nil
	}
	var cand []vsMatch
	s := int32(0)
	for i := 0; i < len(text); i++ {
		s = vs.delta[int(s)<<8|int(text[i])]
		if l := vs.outLen[s]; l > 0 {
			cand = append(cand, vsMatch{i + 1 - int(l), i + 1, int(vs.outIdx[s])})
		}
	}
	if len(cand) == 0 {
		return nil
	}
	sort.Slice(cand, func(a, b int) bool {
		if cand[a].start != cand[b].start {
			return cand[a].start < cand[b].start
		}
		return cand[a].end > cand[b].end
	})
	out := cand[:0]
	last := -1
	for _, m := range cand {
		if m.start < last {
			continue
		}
		out = append(out, m)
		last = m.end
	}
	return out
}
