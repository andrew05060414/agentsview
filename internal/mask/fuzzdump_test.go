package mask

import (
	"bufio"
	"encoding/json"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// TestDumpDifferential writes randomly composed inputs and this package's output
// as JSONL when MASK_DIFF_OUT is set. tools/oc-parity/diff.ts replays them
// through OpenCodex's redactPlainText and compares byte for byte. Only
// constructs both sides implement identically are generated (no bare or
// non-random credential-named values, no extra AgentsView-only vendor formats).
func TestDumpDifferential(t *testing.T) {
	out := os.Getenv("MASK_DIFF_OUT")
	if out == "" {
		t.Skip("MASK_DIFF_OUT not set")
	}
	secrets := [][]string{
		{"AKIA", "IOSFODNN7", "EXAMPLE"},
		{"AIza", "SyD3mo", "-abcdefghijklmnopqrstuvwxyz01"},
		{"sk-", "proj-", "AbCdEf0123456789", "GhIjKlMn"},
		{"sk-", "ant-", "api03-", "Zx9Qw8Er7", "Ty6Ui5Op4", "As3Df2Gh1Jk0"},
		{"ghp_", "ABCDEFGHIJKLMNOP", "QRSTUVWXYZ0123456789"},
		{"github_pat_", "11ABCDEFG0", "abcdefghijklmnopqrstuvwxyz"},
		{"eyJ", "hbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", "dBjftJeZ4CVPmB92K27uhbUJU1p1r"},
		{"tok_", "Zq81Lm40Xv73Bn55Pw"},
	}
	templates := []string{
		"%s", "x%s", "%sx", "a %s b", "Authorization: Bearer %s", "bearer %s.", "Bearer %s\r\n",
		"postgres://u:%s@h:5432/d", "https://bot:%s@example.org/p", "{\"k\":\"%s\"}", "'%s'", "\"%s\"",
		"API_KEY=%s", "db_password=%s;", "{\"client_secret\": \"%s\"}", "?access_token=%s&x=1",
		"%s %s", "%s,%s", "路径 %s 结尾", "line1\n%s\nline3", "\t%s\t", "(%s)", "[%s]", "%s.", "%s-", "-%s",
	}
	randomValue := []string{"Xk3mP9qL2vN8wR5t", "Tr0ub4dor3xYz", "aB3dE6gH9jK2mN5p"}
	rng := rand.New(rand.NewSource(20261027))
	m := newTestMasker(t, Options{})
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	enc := json.NewEncoder(w)
	for i := 0; i < 4000; i++ {
		var parts []string
		for n := 1 + rng.Intn(3); n > 0; n-- {
			tpl := templates[rng.Intn(len(templates))]
			var val string
			if strings.Contains(tpl, "password=") || strings.Contains(tpl, "client_secret") ||
				strings.Contains(tpl, "access_token") || strings.HasPrefix(tpl, "API_KEY=") {
				val = randomValue[rng.Intn(len(randomValue))]
			} else {
				val = strings.Join(secrets[rng.Intn(len(secrets))], "")
			}
			s := tpl
			for strings.Contains(s, "%s") {
				s = strings.Replace(s, "%s", val, 1)
				if strings.Contains(s, "%s") {
					val = strings.Join(secrets[rng.Intn(len(secrets))], "")
				}
			}
			parts = append(parts, s)
		}
		// Parts are always separated: gluing a value to the next part would let a
		// base64 '=' pad (accepted here, not by OpenCodex) swallow the next '='.
		in := strings.Join(parts, []string{" ", "\n", ", ", "\t"}[rng.Intn(4)])
		if err := enc.Encode(map[string]string{"in": in, "out": m.Text(in)}); err != nil {
			t.Fatal(err)
		}
	}
}
