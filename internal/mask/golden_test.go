package mask

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

type goldenFile struct {
	Version int          `json:"version"`
	Cases   []goldenCase `json:"cases"`
}

type goldenCase struct {
	ID   string            `json:"id"`
	Tier string            `json:"tier"`
	OC   string            `json:"oc"`
	In   []json.RawMessage `json:"in"`
	Out  []json.RawMessage `json:"out"`
}

// resolveElems joins the elements of a golden template. A string is literal
// text; {"j": [...]} is a secret value assembled at run time from fragments;
// {"ph": [...]} is that value's placeholder.
func resolveElems(t *testing.T, elems []json.RawMessage) string {
	t.Helper()
	var b strings.Builder
	for _, raw := range elems {
		var lit string
		if err := json.Unmarshal(raw, &lit); err == nil {
			b.WriteString(lit)
			continue
		}
		var obj struct {
			J  []string `json:"j"`
			PH []string `json:"ph"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("bad golden element %s: %v", raw, err)
		}
		switch {
		case obj.J != nil:
			b.WriteString(strings.Join(obj.J, ""))
		case obj.PH != nil:
			b.WriteString(Placeholder(strings.Join(obj.PH, "")))
		default:
			t.Fatalf("bad golden element %s", raw)
		}
	}
	return b.String()
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if g.Version != 1 || len(g.Cases) == 0 {
		t.Fatalf("unexpected golden file: version=%d cases=%d", g.Version, len(g.Cases))
	}
	return g
}

func newTestMasker(t *testing.T, o Options) *Masker {
	t.Helper()
	m, err := NewDefault(o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestGolden runs every case of testdata/golden.json. The "parity" cases are the
// ones the OpenCodex side (tools/oc-parity) must also reproduce byte for byte.
func TestGolden(t *testing.T) {
	m := newTestMasker(t, Options{})
	g := loadGolden(t)
	for _, c := range g.Cases {
		t.Run(c.ID, func(t *testing.T) {
			in := resolveElems(t, c.In)
			want := resolveElems(t, c.Out)
			got := m.Text(in)
			if got != want {
				t.Errorf("case %s (%s): output differs from golden (lengths in=%d got=%d want=%d)",
					c.ID, c.Tier, len(in), len(got), len(want))
			}
			// Idempotent: masking masked text changes nothing.
			if again := m.Text(got); again != got {
				t.Errorf("case %s: not idempotent", c.ID)
			}
		})
	}
}

func TestGoldenCoverage(t *testing.T) {
	g := loadGolden(t)
	parity, goOnly := 0, 0
	for _, c := range g.Cases {
		switch c.Tier {
		case "parity":
			parity++
		case "go":
			goOnly++
			if c.OC != "differs" && c.OC != "unchanged" {
				t.Errorf("go-only case %s needs oc = differs|unchanged", c.ID)
			}
		default:
			t.Errorf("case %s: unknown tier %q", c.ID, c.Tier)
		}
	}
	if parity < 40 || goOnly < 10 {
		t.Errorf("golden file too small: parity=%d go=%d", parity, goOnly)
	}
}

func TestPlaceholderShape(t *testing.T) {
	p := Placeholder("any value")
	if !strings.HasPrefix(p, "{{SECRET_") || !strings.HasSuffix(p, "}}") || len(p) != len("{{SECRET_}}")+8 {
		t.Fatalf("unexpected placeholder shape %q", p)
	}
	if p != Placeholder("any value") || p == Placeholder("any value2") {
		t.Fatal("placeholder must be deterministic and value-specific")
	}
	if !IsPlaceholder(p) || IsPlaceholder("{{SECRET_zz}}") {
		t.Fatal("IsPlaceholder wrong")
	}
	if FP8("abc") != "ba7816bf" { // sha256("abc") starts with ba7816bf
		t.Fatalf("FP8 is not the sha256 prefix: %s", FP8("abc"))
	}
}

func TestExactAndClosure(t *testing.T) {
	exact := "Zq8" + "Lm4Xv" + "7Bn5Pw2#K9"
	closure := "Hd2" + "Fg6Js" + "9Kl3Mn1QrT"
	m := newTestMasker(t, Options{
		Exact:   []ExactValue{{Label: "unit", Value: exact, Strict: true}},
		Closure: []string{closure},
	})
	text := "bare " + exact + " and " + closure + " and again " + exact + "."
	got, hits := m.Mask(text)
	want := "bare " + Placeholder(exact) + " and " + Placeholder(closure) + " and again " + Placeholder(exact) + "."
	if got != want {
		t.Fatalf("literal replacement wrong: %q", got)
	}
	if len(hits) != 3 || hits[0].Rule != "exact:unit" || hits[1].Rule != "closure" {
		t.Fatalf("unexpected hits: %v", hits)
	}
	// Longest value wins when one value contains another.
	long := exact + "XYZ"
	m2 := newTestMasker(t, Options{Exact: []ExactValue{
		{Label: "a", Value: exact}, {Label: "b", Value: long},
	}})
	if got := m2.Text("v=" + long + " w=" + exact); got != "v="+Placeholder(long)+" w="+Placeholder(exact) {
		t.Fatalf("longest-first failed: %q", got)
	}
	// Allow-list keeps a value.
	m3 := newTestMasker(t, Options{Exact: []ExactValue{{Label: "a", Value: exact}}, Allow: map[string]struct{}{exact: {}}})
	if got := m3.Text("v=" + exact); got != "v="+exact {
		t.Fatalf("allow value was masked: %q", got)
	}
}

func TestRegistryTokens(t *testing.T) {
	val := "Rg5" + "Hy7Uj" + "1Ik3Ol9Pa"
	reg := NewRegistry()
	reg.Add("unit", val)
	m := newTestMasker(t, Options{Registry: reg})
	for _, text := range []string{
		"x " + val + " y",
		"NAME=" + val + "\n",
		`"k":"` + val + `",`,
		"https://h/p/" + val + "?a=1",
		"a:" + val,
		"the key is " + val + ".",
		"**" + val + "**",
		"(see " + val + ").",
		"--token=" + val + "!",
	} {
		got := m.Text(text)
		if strings.Contains(got, val) || !strings.Contains(got, Placeholder(val)) {
			t.Errorf("registry token not masked in %q -> %q", "<text>", got)
		}
	}
	if got := m.Text("prefix" + val + "suffix"); got != "prefix"+val+"suffix" {
		t.Errorf("value embedded in a larger token must not match: %q", got)
	}
	// Round trip through the file: only hashes are stored.
	path := t.TempDir() + "/reg.jsonl"
	if err := reg.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), val) || strings.Contains(string(raw), FP8(val)) && false {
		t.Fatal("registry file contains the plaintext value")
	}
	back, err := LoadRegistry(path)
	if err != nil || back.Len() != 1 {
		t.Fatalf("reload: %v len=%d", err, back.Len())
	}
	m2 := newTestMasker(t, Options{Registry: back})
	if got := m2.Text("x " + val); got != "x "+Placeholder(val) {
		t.Fatalf("reloaded registry did not mask: %q", got)
	}
}

func TestHitDoesNotPrintValue(t *testing.T) {
	val := "Ab3" + "Cd4Ef" + "5Gh6Ij7Kl8"
	m := newTestMasker(t, Options{Exact: []ExactValue{{Label: "l", Value: val}}})
	_, hits := m.Mask("v " + val)
	for _, f := range []string{fmt.Sprint(hits), fmt.Sprintf("%v %+v %#v", hits[0], hits[0], hits[0])} {
		if strings.Contains(f, val) {
			t.Fatal("a Hit printed the secret value")
		}
	}
}

func TestMaskedTextIsStable(t *testing.T) {
	// Masking must not depend on call history or on other texts.
	m := newTestMasker(t, Options{})
	in := "Authorization: Bearer " + "tok_" + "Zq81Lm40Xv73Bn55Pw" + " and API_KEY=" + "Xk3mP9qL" + "2vN8wR5t"
	a, b := m.Text(in), m.Text(in)
	if a != b || a == in {
		t.Fatal("masking is not deterministic")
	}
}

func TestExactValueInsideJSONString(t *testing.T) {
	val := "Pa" + `ss"wo\rd` + "9Zq" + "Xv"
	m := newTestMasker(t, Options{Exact: []ExactValue{{Label: "unit", Value: val, Strict: true}}})
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(val)
	in := `{"cmd":"login ` + escaped + ` now"}`
	got := m.Text(in)
	want := `{"cmd":"login ` + Placeholder(val) + ` now"}`
	if got != want {
		t.Fatalf("escaped exact value not masked as the original: %q", got)
	}
	if got := m.Text("plain " + val); got != "plain "+Placeholder(val) {
		t.Fatalf("plain form lost: %q", got)
	}
}

// The keyword-anchored search must find exactly what scanning the whole text finds.
func TestAnchoredSearchEqualsFullScan(t *testing.T) {
	m := newTestMasker(t, Options{})
	frags := []string{
		"api_key", "API_KEY", "token", "Token", "my-secret", "password", "passwd", "pwd", "cookie", "credential", "bearer",
		"key", "x", "_", "-", "=", ":", " = ", ": ", "\"", "'", " ", "\n", ",", ".", "/", "{", "}", "[", "]", "&", "?",
		"Xk3mP9qL2vN8wR5t", "short", "aB3dE6gH9jK2mN5p==", "0123456789abcdef0123456789abcdef", "my_value_here", "https://h/p",
		"AKIA" + "IOSFODNN7EXAMPLE", "Bearer ", "monkey", "public_key", "session_cookie",
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := 1 + rng.Intn(12); n > 0; n-- {
			b.WriteString(frags[rng.Intn(len(frags))])
		}
		in := b.String()
		disableAnchoredForTest = false
		fast := m.Text(in)
		disableAnchoredForTest = true
		full := m.Text(in)
		disableAnchoredForTest = false
		if fast != full {
			t.Fatalf("anchored and full scans differ for input #%d (len %d)", i, len(in))
		}
	}
}
