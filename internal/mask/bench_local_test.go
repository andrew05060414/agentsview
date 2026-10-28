package mask

import (
	"database/sql"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// TestLocalThroughput measures masking speed per rule on real stored text. It
// is skipped unless MASK_BENCH_DB points at a sessions.db (opened read-only).
func TestLocalThroughput(t *testing.T) {
	path := os.Getenv("MASK_BENCH_DB")
	if path == "" {
		t.Skip("MASK_BENCH_DB not set")
	}
	conn, err := sql.Open("sqlite3", "file:"+strings.ReplaceAll(path, "\\", "/")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT content FROM messages WHERE id % 37 = 0 AND length(content) > 0 LIMIT 20000`)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	total := 0
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, s)
		total += len(s)
	}
	rows.Close()
	t.Logf("sample: %d texts, %.1f MB", len(texts), float64(total)/1e6)

	rs, _ := LoadRules()
	// whole masker, rules only
	m, _ := New(rs, Options{})
	start := time.Now()
	for _, s := range texts {
		m.Text(s)
	}
	d := time.Since(start)
	t.Logf("all rules: %.2fs = %.1f MB/s", d.Seconds(), float64(total)/1e6/d.Seconds())

	type res struct {
		id string
		d  time.Duration
	}
	var out []res
	for _, spec := range rs.Rules {
		one := *rs
		one.Rules = []RuleSpec{spec}
		mm, err := New(&one, Options{})
		if err != nil {
			t.Fatal(err)
		}
		st := time.Now()
		for _, s := range texts {
			mm.Text(s)
		}
		out = append(out, res{spec.ID, time.Since(st)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].d > out[j].d })
	for _, r := range out[:6] {
		t.Logf("rule %-28s %.2fs", r.id, r.d.Seconds())
	}
}
