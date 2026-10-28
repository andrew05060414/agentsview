// ABOUTME: One-shot backfill that masks secrets already stored in the archive.
// ABOUTME: Shares internal/mask with the ingestion hook; reports carry no values.
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/mask"
)

// MaskBackfillOptions configures MaskBackfill.
type MaskBackfillOptions struct {
	Rules   *mask.RuleSet
	Exact   []mask.ExactValue // strict values only; plaintext stays in memory
	Apply   bool              // false: compute and count, write nothing
	Workers int
	Log     func(format string, args ...any)
	// RegistryOut, when set, receives the sha256 registry of the exact and
	// closure values (hashes only).
	RegistryOut string
	// ScanFile, when set, is scanned for the run's literal values (exact and
	// admitted closure) once they are known, so a dry-run against the original
	// archive can measure the residue in an already-masked copy.
	ScanFile string
	// LocationsOut, when set, receives a CSV of every location that has a
	// masked span (session, table, ordinal, call index, pass). Locations are
	// ids and coordinates, not values; it lets a dry-run be reconciled with an
	// earlier scan.
	LocationsOut string
}

// MaskBackfillReport holds counts only. Nothing in it can reveal a value: values
// are identified by their 8-hex fingerprint inside Exact and by counts elsewhere.
type MaskBackfillReport struct {
	Apply         bool                   `json:"apply"`
	StartedAt     string                 `json:"started_at"`
	Seconds       float64                `json:"seconds"`
	RulesID       string                 `json:"rules_id"`
	ExactLoaded   int                    `json:"exact_values_loaded"`
	ClosureValues int                    `json:"closure_values"`
	ClosureCand   int                    `json:"closure_candidates"`
	ClosureFPs    map[string]string      `json:"closure_fp8_origin,omitempty"`
	ClosureDrop   []MaskClosureValue     `json:"closure_dropped_too_common,omitempty"`
	Rows          map[string]int         `json:"rows_scanned"`
	RulesOnly     MaskStats              `json:"pass1_rules_and_exact"`
	Final         MaskStats              `json:"pass2_with_closure"`
	Updates       map[string]int         `json:"rows_updated,omitempty"`
	Findings      MaskFindingsReport     `json:"secret_findings,omitempty"`
	Checks        map[string]interface{} `json:"checks,omitempty"`

	literals []string // exact + admitted closure values; memory only, never serialized
}

// LiteralValues returns the plaintext exact and closure values used by the run,
// for a raw-file residue check. They must never be logged or written.
func (r *MaskBackfillReport) LiteralValues() []string { return r.literals }

// MaskFindingsReport describes the secret_findings rewrite.
type MaskFindingsReport struct {
	Rows            int `json:"rows"`
	Rewritten       int `json:"rewritten"`
	UnresolvedBlank int `json:"unresolved_set_to_generic"`
}

// MaskStats mirrors the numbers of the 2026-10-27 scan report: sessions,
// locations (session+table+ordinal+call index), spans and distinct values.
type MaskStats struct {
	Sessions       int                       `json:"sessions"`
	Locations      int                       `json:"locations"`
	Spans          int                       `json:"spans"`
	DistinctValues int                       `json:"distinct_values"`
	RowsChanged    int                       `json:"rows_changed"`
	ByField        map[string]MaskFieldStat  `json:"by_field"`
	ByRule         map[string]MaskRuleStat   `json:"by_rule"`
	Excluded       map[string]int            `json:"gate_excluded_spans"`
	ExactByLabel   map[string]MaskRuleStat   `json:"exact_by_label"`
	ExactValues    map[string]MaskExactValue `json:"exact_values,omitempty"`
	ClosureTop     []MaskClosureValue        `json:"closure_top,omitempty"`
	// ClosureProfile groups the admitted closure values by character classes,
	// entropy and length: "class|entropy bucket|length bucket" -> counts.
	ClosureProfile map[string]MaskProfileCell `json:"closure_profile,omitempty"`
}

// MaskProfileCell is one cell of the closure profile.
type MaskProfileCell struct {
	Values int `json:"values"`
	Spans  int `json:"spans"`
}

// MaskClosureValue is one value of the closure with its occurrence counts
// (fingerprint, length and the rule that first confirmed it; never the value).
type MaskClosureValue struct {
	FP       string `json:"fp8"`
	Len      int    `json:"len"`
	Origin   string `json:"origin_rule"`
	Class    string `json:"class,omitempty"`
	Entropy  string `json:"entropy,omitempty"`
	Spans    int    `json:"spans"`
	Sessions int    `json:"sessions"`
}

// Closure admission limits. A value that a rule confirmed in a credential
// context is promoted to "replace everywhere" only if it is rare enough to be a
// secret rather than a common identifier that happened to follow a key-like
// name. Values above the limits are kept out of the closure (they are still
// masked inside the credential context that confirmed them) and listed in the
// report.
const (
	closureMaxSessions = 150
	closureMaxSpans    = 3000
)

// MaskFieldStat counts per table.field.
type MaskFieldStat struct {
	Rows  int `json:"rows_changed"`
	Spans int `json:"spans"`
}

// MaskRuleStat counts per rule (or exact-value label).
type MaskRuleStat struct {
	Spans    int `json:"spans"`
	Sessions int `json:"sessions"`
	Values   int `json:"distinct_values"`
}

// MaskExactValue identifies one exact value by label, length and fingerprint.
type MaskExactValue struct {
	Label    string `json:"label"`
	Len      int    `json:"len"`
	Spans    int    `json:"spans"`
	Sessions int    `json:"sessions"`
}

type maskLocKey struct {
	session, table string
	ord, idx       int64
}

type maskAccum struct {
	sessions map[string]struct{}
	locs     map[maskLocKey]string // location -> first rule that fired there
	spans    int
	values   map[string]struct{}
	byField  map[string]*MaskFieldStat
	rule     map[string]*maskRuleAcc
	label    map[string]*maskRuleAcc
	exactFP  map[string]*maskExactAcc
	closureF map[string]*maskExactAcc // pass 2: occurrences per closure value (by fingerprint)
	origin   map[string]string        // pass 1: fingerprint -> rule that confirmed the closure value
	oinfo    map[string][2]string     // pass 1: fingerprint -> class, entropy
	excluded map[string]int
	closure  map[string]struct{}
	rows     int
}

type maskRuleAcc struct {
	spans    int
	sessions map[string]struct{}
	values   map[string]struct{}
}

type maskExactAcc struct {
	label    string
	n        int
	spans    int
	sessions map[string]struct{}
}

func newMaskAccum(collectClosure bool) *maskAccum {
	a := &maskAccum{
		sessions: map[string]struct{}{}, locs: map[maskLocKey]string{}, values: map[string]struct{}{},
		byField: map[string]*MaskFieldStat{}, rule: map[string]*maskRuleAcc{}, label: map[string]*maskRuleAcc{},
		exactFP: map[string]*maskExactAcc{}, closureF: map[string]*maskExactAcc{},
		origin: map[string]string{}, oinfo: map[string][2]string{}, excluded: map[string]int{},
	}
	if collectClosure {
		a.closure = map[string]struct{}{}
	}
	return a
}

func (a *maskAccum) accRule(m map[string]*maskRuleAcc, k string) *maskRuleAcc {
	r := m[k]
	if r == nil {
		r = &maskRuleAcc{sessions: map[string]struct{}{}, values: map[string]struct{}{}}
		m[k] = r
	}
	return r
}

// observe records the hits of one text field. changed tells whether the text differs after masking.
func (a *maskAccum) observe(table, field string, session string, ord, idx int64, hits []mask.Hit, changed bool) {
	if len(hits) == 0 {
		return
	}
	derived := table == "pg_sync_state" || table == "session_signal_state"
	any := false
	for _, h := range hits {
		if h.Excluded != "" {
			a.excluded[h.Rule+":"+h.Excluded]++
			continue
		}
		any = true
		a.spans++
		a.values[h.FP] = struct{}{}
		r := a.accRule(a.rule, h.Rule)
		r.spans++
		r.sessions[session] = struct{}{}
		r.values[h.FP] = struct{}{}
		if strings.HasPrefix(h.Rule, "exact:") {
			l := a.accRule(a.label, strings.TrimPrefix(h.Rule, "exact:"))
			l.spans++
			l.sessions[session] = struct{}{}
			l.values[h.FP] = struct{}{}
			e := a.exactFP[h.FP]
			if e == nil {
				e = &maskExactAcc{label: strings.TrimPrefix(h.Rule, "exact:"), n: h.Len, sessions: map[string]struct{}{}}
				a.exactFP[h.FP] = e
			}
			e.spans++
			e.sessions[session] = struct{}{}
		}
		if a.closure != nil {
			if v, ok := h.ClosureValue(); ok {
				a.closure[v] = struct{}{}
				a.origin[h.FP] = h.Rule
				cl, en := mask.ValueClass(v)
				a.oinfo[h.FP] = [2]string{cl, fmt.Sprintf("%.1f", en)}
			}
		}
		if h.Rule == "closure" {
			e := a.closureF[h.FP]
			if e == nil {
				e = &maskExactAcc{n: h.Len, sessions: map[string]struct{}{}}
				a.closureF[h.FP] = e
			}
			e.spans++
			e.sessions[session] = struct{}{}
		}
	}
	if any {
		if !derived {
			a.sessions[session] = struct{}{}
		}
		firstRule := ""
		for _, h := range hits {
			if h.Excluded == "" {
				firstRule = h.Rule
				break
			}
		}
		if _, seen := a.locs[maskLocKey{session, table, ord, idx}]; !seen {
			a.locs[maskLocKey{session, table, ord, idx}] = firstRule
		}
		f := a.byField[table+"."+field]
		if f == nil {
			f = &MaskFieldStat{}
			a.byField[table+"."+field] = f
		}
		if changed {
			f.Rows++
		}
		for _, h := range hits {
			if h.Excluded == "" {
				f.Spans++
			}
		}
	}
}

func (a *maskAccum) merge(o *maskAccum) {
	for k := range o.sessions {
		a.sessions[k] = struct{}{}
	}
	for k, v := range o.locs {
		if _, seen := a.locs[k]; !seen {
			a.locs[k] = v
		}
	}
	for k := range o.values {
		a.values[k] = struct{}{}
	}
	a.spans += o.spans
	a.rows += o.rows
	for k, v := range o.byField {
		f := a.byField[k]
		if f == nil {
			f = &MaskFieldStat{}
			a.byField[k] = f
		}
		f.Rows += v.Rows
		f.Spans += v.Spans
	}
	mergeRules := func(dst, src map[string]*maskRuleAcc) {
		for k, v := range src {
			d := a.accRule(dst, k)
			d.spans += v.spans
			for s := range v.sessions {
				d.sessions[s] = struct{}{}
			}
			for s := range v.values {
				d.values[s] = struct{}{}
			}
		}
	}
	mergeRules(a.rule, o.rule)
	mergeRules(a.label, o.label)
	for k, v := range o.exactFP {
		e := a.exactFP[k]
		if e == nil {
			e = &maskExactAcc{label: v.label, n: v.n, sessions: map[string]struct{}{}}
			a.exactFP[k] = e
		}
		e.spans += v.spans
		for s := range v.sessions {
			e.sessions[s] = struct{}{}
		}
	}
	for k, v := range o.closureF {
		e := a.closureF[k]
		if e == nil {
			e = &maskExactAcc{n: v.n, sessions: map[string]struct{}{}}
			a.closureF[k] = e
		}
		e.spans += v.spans
		for s := range v.sessions {
			e.sessions[s] = struct{}{}
		}
	}
	for k, v := range o.origin {
		a.origin[k] = v
	}
	for k, v := range o.oinfo {
		a.oinfo[k] = v
	}
	for k, v := range o.excluded {
		a.excluded[k] += v
	}
	if a.closure != nil {
		for k := range o.closure {
			a.closure[k] = struct{}{}
		}
	}
}

func (a *maskAccum) stats() MaskStats {
	st := MaskStats{
		Sessions: len(a.sessions), Locations: len(a.locs), Spans: a.spans, DistinctValues: len(a.values),
		ByField: map[string]MaskFieldStat{}, ByRule: map[string]MaskRuleStat{}, Excluded: a.excluded,
		ExactByLabel: map[string]MaskRuleStat{}, ExactValues: map[string]MaskExactValue{},
	}
	for k, v := range a.byField {
		st.ByField[k] = *v
		st.RowsChanged += v.Rows
	}
	for k, v := range a.rule {
		st.ByRule[k] = MaskRuleStat{Spans: v.spans, Sessions: len(v.sessions), Values: len(v.values)}
	}
	for k, v := range a.label {
		st.ExactByLabel[k] = MaskRuleStat{Spans: v.spans, Sessions: len(v.sessions), Values: len(v.values)}
	}
	for fp, e := range a.exactFP {
		st.ExactValues[fp] = MaskExactValue{Label: e.label, Len: e.n, Spans: e.spans, Sessions: len(e.sessions)}
	}
	for fp, e := range a.closureF {
		st.ClosureTop = append(st.ClosureTop, MaskClosureValue{FP: fp, Len: e.n, Origin: a.origin[fp], Class: a.oinfo[fp][0], Entropy: a.oinfo[fp][1], Spans: e.spans, Sessions: len(e.sessions)})
	}
	st.ClosureProfile = map[string]MaskProfileCell{}
	for fp, e := range a.closureF {
		info := a.oinfo[fp]
		en := 0.0
		fmt.Sscanf(info[1], "%f", &en)
		eb := "<3.0"
		switch {
		case en >= 4.0:
			eb = ">=4.0"
		case en >= 3.5:
			eb = "3.5-4.0"
		case en >= 3.0:
			eb = "3.0-3.5"
		}
		lb := "<16"
		switch {
		case e.n >= 32:
			lb = ">=32"
		case e.n >= 20:
			lb = "20-31"
		case e.n >= 16:
			lb = "16-19"
		}
		k := info[0] + "|" + eb + "|" + lb
		c := st.ClosureProfile[k]
		c.Values++
		c.Spans += e.spans
		st.ClosureProfile[k] = c
	}
	sort.Slice(st.ClosureTop, func(i, j int) bool { return st.ClosureTop[i].Spans > st.ClosureTop[j].Spans })
	if len(st.ClosureTop) > 40 {
		st.ClosureTop = st.ClosureTop[:40]
	}
	return st
}

// maskRow is one row of a table being masked. text slots follow maskTable.fields.
type maskRow struct {
	key     int64
	session string
	ord     int64
	idx     int64
	text    [3]sql.NullString
	length  sql.NullInt64
}

type maskTable struct {
	name    string
	fields  []string // text columns, in select order (max 3)
	keyCol  string   // column used in WHERE of the UPDATE and for paging
	selectQ string   // must select: key, session, ord, idx, t0, t1, t2, length ... WHERE key > ? ORDER BY key LIMIT ?
	lenCol  string   // length column kept in step with text ("" = none)
	lenFrom int      // index of the field the length belongs to
	// lenAlways: after a change set length = len(new text) (stored lengths are
	// derived from the text by construction); otherwise only when the old
	// length equalled len(old text).
	lenAlways bool
	// blob: the column holds a BLOB that is UTF-8 JSON; it is written back as a BLOB.
	blob bool
	// derived: bookkeeping that copies session text (the session column of the
	// select holds a label, not a session id, and is left out of session counts).
	derived bool
}

var maskTables = []maskTable{
	{
		name: "messages", fields: []string{"content", "thinking_text", "dialogue_text"}, keyCol: "id",
		selectQ: `SELECT id, session_id, ordinal, -1, content, thinking_text, dialogue_text, content_length
		          FROM messages WHERE id > ? ORDER BY id LIMIT ?`,
		lenCol: "content_length", lenFrom: 0,
	},
	{
		name: "tool_calls", fields: []string{"input_json", "result_content"}, keyCol: "id",
		selectQ: `SELECT tc.id, tc.session_id, COALESCE(m.ordinal, -1), tc.call_index, tc.input_json, tc.result_content, NULL, tc.result_content_length
		          FROM tool_calls tc LEFT JOIN messages m ON m.id = tc.message_id
		          WHERE tc.id > ? ORDER BY tc.id LIMIT ?`,
		lenCol: "result_content_length", lenFrom: 1, lenAlways: true,
	},
	{
		name: "tool_result_events", fields: []string{"content"}, keyCol: "id",
		selectQ: `SELECT id, session_id, tool_call_message_ordinal, call_index, content, NULL, NULL, content_length
		          FROM tool_result_events WHERE id > ? ORDER BY id LIMIT ?`,
		lenCol: "content_length", lenFrom: 0, lenAlways: true,
	},
	{
		name: "sessions", fields: []string{"first_message", "display_name", "session_name"}, keyCol: "rowid",
		selectQ: `SELECT rowid, id, -1, -1, first_message, display_name, session_name, NULL
		          FROM sessions WHERE rowid > ? ORDER BY rowid LIMIT ?`,
	},
	{
		name: "recall_entries", fields: []string{"title", "body", "trigger"}, keyCol: "rowid",
		selectQ: `SELECT rowid, COALESCE(source_session_id, ''), -1, -1, title, body, trigger, NULL
		          FROM recall_entries WHERE rowid > ? ORDER BY rowid LIMIT ?`,
	},
	{
		// PostgreSQL push bookkeeping: per-session fingerprints embed session text.
		name: "pg_sync_state", fields: []string{"value"}, keyCol: "rowid", derived: true,
		selectQ: `SELECT rowid, '~pg_sync_state', -1, -1, value, NULL, NULL, NULL
		          FROM pg_sync_state WHERE rowid > ? ORDER BY rowid LIMIT ?`,
	},
	{
		// Incremental signal seeds copy tool input text (for example the last retried input).
		name: "session_signal_state", fields: []string{"state"}, keyCol: "rowid", derived: true, blob: true,
		selectQ: `SELECT rowid, session_id, -1, -1, CAST(state AS TEXT), NULL, NULL, NULL
		          FROM session_signal_state WHERE rowid > ? ORDER BY rowid LIMIT ?`,
	},
	{
		name: "recall_evidence", fields: []string{"snippet"}, keyCol: "id",
		selectQ: `SELECT id, COALESCE(session_id, ''), COALESCE(message_start_ordinal, -1), -1, snippet, NULL, NULL, NULL
		          FROM recall_evidence WHERE id > ? ORDER BY id LIMIT ?`,
	},
}

const maskBatchRows = 2000

// scanMaskTable streams the rows of one table in key order, in batches.
func scanMaskTable(ctx context.Context, rd Reader, t maskTable, fn func(rows []maskRow) error) (int, error) {
	var last int64 = -1 << 62
	total := 0
	for {
		rs, err := rd.Query(ctx, t.selectQ, last, maskBatchRows)
		if err != nil {
			return total, fmt.Errorf("masking %s: %w", t.name, err)
		}
		batch := make([]maskRow, 0, maskBatchRows)
		for rs.Next() {
			var r maskRow
			if err := rs.Scan(&r.key, &r.session, &r.ord, &r.idx, &r.text[0], &r.text[1], &r.text[2], &r.length); err != nil {
				_ = rs.Close()
				return total, fmt.Errorf("masking %s: scan: %w", t.name, err)
			}
			batch = append(batch, r)
		}
		if err := rs.Err(); err != nil {
			_ = rs.Close()
			return total, err
		}
		_ = rs.Close()
		if len(batch) == 0 {
			return total, nil
		}
		last = batch[len(batch)-1].key
		total += len(batch)
		if err := fn(batch); err != nil {
			return total, err
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

func parallelFor(n, workers int, fn func(lo, hi int)) {
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}
	if workers <= 1 {
		fn(0, n)
		return
	}
	var wg sync.WaitGroup
	chunk := (n + workers - 1) / workers
	for lo := 0; lo < n; lo += chunk {
		hi := min(lo+chunk, n)
		wg.Add(1)
		go func() { defer wg.Done(); fn(lo, hi) }()
	}
	wg.Wait()
}

type maskedRow struct {
	row     *maskRow
	out     [3]string
	changed [3]bool
}

// MaskBackfill masks the stored text of messages, tool calls, tool result
// events, sessions and recall tables. Pass 1 applies the rules and the exact
// values and gathers the value closure; pass 2 applies everything (closure
// included) and, when opts.Apply is set, writes the changes. secret_findings
// is rewritten to placeholder form, and the full-text indexes are rebuilt so no
// plaintext token survives in them.
func (db *DB) MaskBackfill(ctx context.Context, opts MaskBackfillOptions) (*MaskBackfillReport, error) {
	if opts.Rules == nil {
		return nil, errors.New("mask backfill: rules required")
	}
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = max(2, runtime.NumCPU()-2)
	}
	start := time.Now()
	rep := &MaskBackfillReport{
		Apply: opts.Apply, StartedAt: start.UTC().Format(time.RFC3339), RulesID: opts.Rules.ID,
		ExactLoaded: len(opts.Exact), Rows: map[string]int{}, Updates: map[string]int{},
	}
	rd := db.Reader()

	base, err := mask.New(opts.Rules, mask.Options{Exact: opts.Exact})
	if err != nil {
		return nil, err
	}

	// Pass 1: rules + exact values; collect the value closure.
	p1 := newMaskAccum(true)
	var mu sync.Mutex
	for _, t := range maskTables {
		t := t
		n, err := scanMaskTable(ctx, rd, t, func(rows []maskRow) error {
			parallelFor(len(rows), workers, func(lo, hi int) {
				local := newMaskAccum(true)
				for i := lo; i < hi; i++ {
					r := &rows[i]
					for fi, f := range t.fields {
						if !r.text[fi].Valid || r.text[fi].String == "" {
							continue
						}
						out, hits := base.Mask(r.text[fi].String)
						local.observe(t.name, f, r.session, r.ord, r.idx, hits, out != r.text[fi].String)
					}
				}
				mu.Lock()
				p1.merge(local)
				mu.Unlock()
			})
			return nil
		})
		rep.Rows[t.name] = n
		if err != nil {
			return rep, err
		}
		logf("pass1 %s: %d rows (%.0fs)", t.name, n, time.Since(start).Seconds())
	}
	rep.RulesOnly = p1.stats()
	candidates := make([]string, 0, len(p1.closure))
	for v := range p1.closure {
		candidates = append(candidates, v)
	}
	sort.Strings(candidates)
	rep.ClosureCand = len(candidates)

	// Pass 1.5: how common is each candidate? Drop the ones that are too common.
	closure := candidates
	if len(candidates) > 0 {
		lit, err := mask.NewLiteralMasker(opts.Rules, candidates)
		if err != nil {
			return rep, err
		}
		pc := newMaskAccum(false)
		pc.origin, pc.oinfo = p1.origin, p1.oinfo
		for _, t := range maskTables {
			t := t
			_, err := scanMaskTable(ctx, rd, t, func(rows []maskRow) error {
				parallelFor(len(rows), workers, func(lo, hi int) {
					local := newMaskAccum(false)
					for i := lo; i < hi; i++ {
						r := &rows[i]
						for fi, f := range t.fields {
							if !r.text[fi].Valid || r.text[fi].String == "" {
								continue
							}
							out, hits := lit.Mask(r.text[fi].String)
							local.observe(t.name, f, r.session, r.ord, r.idx, hits, out != r.text[fi].String)
						}
					}
					mu.Lock()
					pc.merge(local)
					mu.Unlock()
				})
				return nil
			})
			if err != nil {
				return rep, err
			}
		}
		common := map[string]bool{}
		for fp, e := range pc.closureF {
			if len(e.sessions) > closureMaxSessions || e.spans > closureMaxSpans {
				common[fp] = true
				rep.ClosureDrop = append(rep.ClosureDrop, MaskClosureValue{FP: fp, Len: e.n, Origin: p1.origin[fp],
					Class: p1.oinfo[fp][0], Entropy: p1.oinfo[fp][1], Spans: e.spans, Sessions: len(e.sessions)})
			}
		}
		sort.Slice(rep.ClosureDrop, func(i, j int) bool { return rep.ClosureDrop[i].Spans > rep.ClosureDrop[j].Spans })
		closure = closure[:0:0]
		for _, v := range candidates {
			if !common[mask.FP8(v)] {
				closure = append(closure, v)
			}
		}
		logf("closure: %d candidates, %d dropped as too common, %d admitted (%.0fs)",
			len(candidates), len(common), len(closure), time.Since(start).Seconds())
	}
	rep.ClosureValues = len(closure)
	rep.ClosureFPs = make(map[string]string, len(closure))
	for _, v := range closure {
		rep.ClosureFPs[mask.FP8(v)] = p1.origin[mask.FP8(v)]
	}
	for _, e := range opts.Exact {
		rep.literals = append(rep.literals, e.Value)
	}
	rep.literals = append(rep.literals, closure...)
	if opts.ScanFile != "" {
		res, err := mask.ScanFile(opts.ScanFile, rep.literals, nil)
		if err != nil {
			return rep, err
		}
		if rep.Checks == nil {
			rep.Checks = map[string]interface{}{}
		}
		rep.Checks["raw_scan_of:"+filepath.Base(opts.ScanFile)] = map[string]any{
			"bytes": res.Bytes, "full_value_hits": sumCounts(res.Full), "fragment_hits": sumCounts(res.Frag),
			"full_by_fp8": res.Full, "fragment_by_fp8": res.Frag,
			"full_pages": res.FullPages, "fragment_pages": res.FragPages,
		}
		logf("raw scan of %s: %d bytes, full-value hits %d, fragment hits %d", filepath.Base(opts.ScanFile),
			res.Bytes, sumCounts(res.Full), sumCounts(res.Frag))
	}

	final, err := mask.New(opts.Rules, mask.Options{Exact: opts.Exact, Closure: closure})
	if err != nil {
		return rep, err
	}
	if opts.RegistryOut != "" {
		reg := mask.NewRegistry()
		for _, e := range opts.Exact {
			reg.Add(e.Label, e.Value)
		}
		for _, v := range closure {
			reg.Add("closure", v)
		}
		if err := reg.Save(opts.RegistryOut); err != nil {
			return rep, err
		}
		logf("registry written: %d hashes", reg.Len())
	}

	// secret_findings must be rewritten from the ORIGINAL text, so resolve it first.
	var findingUpdates []findingUpdate
	if opts.Apply {
		findingUpdates, rep.Findings, err = db.planFindingRewrite(ctx, final)
		if err != nil {
			return rep, err
		}
	}

	// Pass 2: final masker; write when applying.
	p2 := newMaskAccum(false)
	p2.origin, p2.oinfo = p1.origin, p1.oinfo
	for _, t := range maskTables {
		t := t
		_, err := scanMaskTable(ctx, rd, t, func(rows []maskRow) error {
			results := make([]maskedRow, len(rows))
			parallelFor(len(rows), workers, func(lo, hi int) {
				local := newMaskAccum(false)
				for i := lo; i < hi; i++ {
					r := &rows[i]
					results[i].row = r
					for fi, f := range t.fields {
						if !r.text[fi].Valid || r.text[fi].String == "" {
							continue
						}
						out, hits := final.Mask(r.text[fi].String)
						results[i].out[fi] = out
						if out != r.text[fi].String {
							results[i].changed[fi] = true
						}
						local.observe(t.name, f, r.session, r.ord, r.idx, hits, results[i].changed[fi])
					}
				}
				mu.Lock()
				p2.merge(local)
				mu.Unlock()
			})
			if !opts.Apply {
				return nil
			}
			return db.applyMaskBatch(ctx, t, results, rep.Updates)
		})
		if err != nil {
			return rep, err
		}
		logf("pass2 %s done (%.0fs)", t.name, time.Since(start).Seconds())
	}
	rep.Final = p2.stats()
	if opts.LocationsOut != "" {
		if err := writeMaskLocations(opts.LocationsOut, p1, p2); err != nil {
			return rep, err
		}
	}

	if opts.Apply {
		if err := db.applyFindingRewrite(ctx, findingUpdates); err != nil {
			return rep, err
		}
		logf("secret_findings rewritten: %d", len(findingUpdates))
	}
	rep.Seconds = time.Since(start).Seconds()
	return rep, nil
}

func (db *DB) applyMaskBatch(ctx context.Context, t maskTable, rows []maskedRow, updates map[string]int) error {
	var pending []maskedRow
	for _, r := range rows {
		if r.changed[0] || r.changed[1] || r.changed[2] {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	err := db.Update(ctx, func(tx *sql.Tx) error {
		for _, r := range pending {
			var sets []string
			var args []any
			for fi, f := range t.fields {
				if !r.changed[fi] {
					continue
				}
				sets = append(sets, `"`+f+`" = ?`)
				if t.blob {
					args = append(args, []byte(r.out[fi]))
				} else {
					args = append(args, r.out[fi])
				}
			}
			if t.lenCol != "" && r.changed[t.lenFrom] {
				newLen := len(r.out[t.lenFrom])
				oldText := r.row.text[t.lenFrom].String
				if t.lenAlways && newLen > 0 || r.row.length.Valid && int(r.row.length.Int64) == len(oldText) {
					sets = append(sets, `"`+t.lenCol+`" = ?`)
					args = append(args, newLen)
				}
			}
			args = append(args, r.row.key)
			q := "UPDATE " + t.name + " SET " + strings.Join(sets, ", ") + " WHERE " + t.keyCol + " = ?"
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return fmt.Errorf("masking %s row %d: %w", t.name, r.row.key, err)
			}
			if t.name == "tool_result_events" && r.changed[0] {
				// A call whose summary is deduplicated into its only non-empty result
				// event has an empty result_content and keeps the summary length (the
				// event text length) in result_content_length; follow the masked event.
				if _, err := tx.ExecContext(ctx,
					`UPDATE tool_calls SET result_content_length = ?
					 WHERE session_id = ? AND call_index = ?
					   AND message_id = (SELECT id FROM messages WHERE session_id = ? AND ordinal = ?)
					   AND COALESCE(result_content, '') = '' AND COALESCE(result_content_length, 0) = ?
					   AND (SELECT COUNT(*) FROM tool_result_events
					        WHERE session_id = ? AND tool_call_message_ordinal = ? AND call_index = ?
					          AND TRIM(content) <> '') = 1`,
					len(r.out[0]), r.row.session, r.row.idx, r.row.session, r.row.ord, len(r.row.text[0].String),
					r.row.session, r.row.ord, r.row.idx); err != nil {
					return fmt.Errorf("masking tool call summary length %s/%d: %w", r.row.session, r.row.ord, err)
				}
			}
			if t.name == "messages" && r.changed[0] {
				// The conversation export projection keeps sha256 and byte
				// length of the message text; keep it in step with the row.
				sum := sha256.Sum256([]byte(r.out[0]))
				if _, err := tx.ExecContext(ctx,
					`UPDATE conversation_messages SET digest = ?, text_bytes = ?
					 WHERE session_id = ? AND ordinal = ? AND removed = 0 AND digest <> ''`,
					hex.EncodeToString(sum[:]), len(r.out[0]), r.row.session, r.row.ord); err != nil {
					return fmt.Errorf("masking conversation digest %s/%d: %w", r.row.session, r.row.ord, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	updates[t.name] += len(pending)
	return nil
}

type findingUpdate struct {
	id         int64
	redacted   string
	start, end int
}

// planFindingRewrite computes, for each secret_findings row, the placeholder of
// the original span and its new offsets in the masked text. Rows whose source
// text no longer resolves get the generic "{{SECRET_unknown}}" form; either way
// the stored prefix/suffix of the old redacted form disappears.
func (db *DB) planFindingRewrite(ctx context.Context, m *mask.Masker) ([]findingUpdate, MaskFindingsReport, error) {
	var rep MaskFindingsReport
	rd := db.Reader()
	rs, err := rd.Query(ctx, `SELECT id, session_id, location_kind, message_ordinal, call_index, event_index, match_start, match_end
	                          FROM secret_findings ORDER BY id`)
	if err != nil {
		return nil, rep, err
	}
	type frow struct {
		id            int64
		session, kind string
		ord           int64
		call, event   sql.NullInt64
		mstart, mend  int
	}
	var all []frow
	for rs.Next() {
		var f frow
		if err := rs.Scan(&f.id, &f.session, &f.kind, &f.ord, &f.call, &f.event, &f.mstart, &f.mend); err != nil {
			_ = rs.Close()
			return nil, rep, err
		}
		all = append(all, f)
	}
	_ = rs.Close()
	rep.Rows = len(all)
	var out []findingUpdate
	for _, f := range all {
		var text sql.NullString
		var qerr error
		switch f.kind {
		case "message":
			qerr = rd.QueryRow(ctx, `SELECT content FROM messages WHERE session_id = ? AND ordinal = ?`, f.session, f.ord).Scan(&text)
		case "tool_input":
			qerr = rd.QueryRow(ctx, `SELECT tc.input_json FROM tool_calls tc JOIN messages m ON m.id = tc.message_id
			                         WHERE tc.session_id = ? AND m.ordinal = ? AND tc.call_index = ?`, f.session, f.ord, f.call).Scan(&text)
		case "tool_result":
			qerr = rd.QueryRow(ctx, `SELECT tc.result_content FROM tool_calls tc JOIN messages m ON m.id = tc.message_id
			                         WHERE tc.session_id = ? AND m.ordinal = ? AND tc.call_index = ?`, f.session, f.ord, f.call).Scan(&text)
		case "tool_result_event":
			qerr = rd.QueryRow(ctx, `SELECT content FROM tool_result_events
			                         WHERE session_id = ? AND tool_call_message_ordinal = ? AND call_index = ? AND event_index = ?`,
				f.session, f.ord, f.call, f.event).Scan(&text)
		default:
			qerr = sql.ErrNoRows
		}
		u := findingUpdate{id: f.id, redacted: mask.PlaceholderPrefix + "unknown" + mask.PlaceholderSuffix, start: f.mstart, end: f.mend}
		if qerr == nil && text.Valid && f.mstart >= 0 && f.mend <= len(text.String) && f.mstart < f.mend {
			span := text.String[f.mstart:f.mend]
			ph := mask.Placeholder(span)
			masked := m.Text(text.String)
			if i := strings.Index(masked, ph); i >= 0 {
				u.start, u.end = i, i+len(ph)
			}
			u.redacted = ph
			rep.Rewritten++
		} else {
			rep.UnresolvedBlank++
		}
		out = append(out, u)
	}
	return out, rep, nil
}

func (db *DB) applyFindingRewrite(ctx context.Context, ups []findingUpdate) error {
	if len(ups) == 0 {
		return nil
	}
	return db.Update(ctx, func(tx *sql.Tx) error {
		for _, u := range ups {
			if _, err := tx.ExecContext(ctx, `UPDATE secret_findings SET redacted_match = ?, match_start = ?, match_end = ? WHERE id = ?`,
				u.redacted, u.start, u.end, u.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// MaskRebuildIndexes rebuilds every full-text index that mirrors masked text so
// the old tokens disappear, then checkpoints the WAL. Run VACUUM afterwards to
// drop the freed pages.
func (db *DB) MaskRebuildIndexes(ctx context.Context) error {
	if err := db.RebuildFTS(ctx); err != nil {
		return err
	}
	return db.Update(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`INSERT INTO recall_entries_fts(recall_entries_fts) VALUES('rebuild')`,
			`INSERT INTO recall_evidence_fts(recall_evidence_fts) VALUES('rebuild')`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
}

// MaskVerify runs the index consistency checks that must hold after a backfill.
// full also runs PRAGMA integrity_check, which reads the whole file.
func (db *DB) MaskVerify(ctx context.Context, full bool) (map[string]string, error) {
	res := map[string]string{}
	run := func(name, q string) {
		err := db.Update(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, q)
			return err
		})
		if err != nil {
			res[name] = "FAIL: " + err.Error()
		} else {
			res[name] = "ok"
		}
	}
	run("messages_fts.integrity-check", `INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', 1)`)
	if db.HasCJKFTS(ctx) {
		run("messages_cjk_fts.integrity-check", `INSERT INTO messages_cjk_fts(messages_cjk_fts, rank) VALUES('integrity-check', 1)`)
	} else {
		res["messages_cjk_fts.integrity-check"] = "skipped: index not available"
	}
	run("recall_entries_fts.integrity-check", `INSERT INTO recall_entries_fts(recall_entries_fts, rank) VALUES('integrity-check', 1)`)
	run("recall_evidence_fts.integrity-check", `INSERT INTO recall_evidence_fts(recall_evidence_fts, rank) VALUES('integrity-check', 1)`)
	if full {
		var s string
		if err := db.Reader().QueryRow(ctx, `PRAGMA integrity_check`).Scan(&s); err != nil {
			res["integrity_check"] = "FAIL: " + err.Error()
		} else {
			res["integrity_check"] = s
		}
	}
	return res, nil
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// MaskResidueHit locates a remaining secret value (or a 12+ character run of
// it) in a text field. Positions and fingerprints only.
type MaskResidueHit struct {
	Table   string `json:"table"`
	Field   string `json:"field"`
	Session string `json:"session"`
	Ordinal int64  `json:"ordinal"`
	Index   int64  `json:"call_index"`
	FP      string `json:"fp8"`
	Full    bool   `json:"full_value"`
	Offset  int    `json:"offset"`
	FieldLn int    `json:"field_len"`
	HitLen  int    `json:"hit_len"`
	Class   string `json:"hit_class"`
	Entropy string `json:"hit_entropy"`
	Before  string `json:"char_class_before"`
	After   string `json:"char_class_after"`
}

// charClass names the kind of one byte for reports: a=letter d=digit s=space/newline p=punctuation.
func charClass(c byte) string {
	switch {
	case c >= '0' && c <= '9':
		return "d"
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return "a"
	case c == ' ' || c == 10 || c == 13 || c == 9:
		return "s"
	}
	return "p" + string(rune(c))
}

// MaskResidue scans every masked text field for the given plaintext values and
// returns where any of them (or fragments) remain.
func (db *DB) MaskResidue(ctx context.Context, values []string, workers int, limit int) ([]MaskResidueHit, error) {
	if workers <= 0 {
		workers = max(2, runtime.NumCPU()-2)
	}
	fs := mask.NewFragmentScanner(values)
	var mu sync.Mutex
	var out []MaskResidueHit
	for _, t := range maskTables {
		t := t
		_, err := scanMaskTable(ctx, db.Reader(), t, func(rows []maskRow) error {
			parallelFor(len(rows), workers, func(lo, hi int) {
				for i := lo; i < hi; i++ {
					r := &rows[i]
					for fi, f := range t.fields {
						if !r.text[fi].Valid || r.text[fi].String == "" {
							continue
						}
						for _, h := range fs.Find(r.text[fi].String) {
							mu.Lock()
							if len(out) < limit {
								s := r.text[fi].String
								cl, en := mask.ValueClass(s[h.Start:h.End])
								before, after := "^", "$"
								if h.Start > 0 {
									before = charClass(s[h.Start-1])
								}
								if h.End < len(s) {
									after = charClass(s[h.End])
								}
								out = append(out, MaskResidueHit{Table: t.name, Field: f, Session: r.session, Ordinal: r.ord,
									Index: r.idx, FP: h.FP, Full: h.Full, Offset: h.Start, FieldLn: len(s),
									HitLen: h.End - h.Start, Class: cl, Entropy: fmt.Sprintf("%.1f", en), Before: before, After: after})
							}
							mu.Unlock()
						}
					}
				}
			})
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func writeMaskLocations(path string, p1, p2 *maskAccum) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"session", "table", "ord", "idx", "pass", "rule"})
	emit := func(a *maskAccum, tag string) {
		for k, rule := range a.locs {
			_ = w.Write([]string{k.session, k.table, strconv.FormatInt(k.ord, 10), strconv.FormatInt(k.idx, 10), tag, rule})
		}
	}
	emit(p1, "p1")
	emit(p2, "p2")
	w.Flush()
	return w.Error()
}
