package mask

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed rules.v1.json
var embeddedRules []byte

// RuleSet is the parsed rules.v1.json. It is the Go side of the rule contract
// shared with OpenCodex; testdata/golden.json is the cross-language check.
type RuleSet struct {
	ID          string `json:"id"`
	Tier        string `json:"tier"`
	Placeholder struct {
		Prefix   string `json:"prefix"`
		Suffix   string `json:"suffix"`
		HexChars int    `json:"hexChars"`
	} `json:"placeholder"`
	Closure struct {
		MinLen int `json:"minLen"`
	} `json:"closure"`
	Rules []RuleSpec `json:"rules"`
}

// RuleSpec is one detector in the rule file.
type RuleSpec struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Origin        string    `json:"origin"`
	Pattern       string    `json:"pattern"`
	Flags         string    `json:"flags"`
	SecretGroup   int       `json:"secretGroup"`
	SecretGroups  []int     `json:"secretGroups"`
	Prefilter     []string  `json:"prefilter"`
	PrefilterFold bool      `json:"prefilterFold"`
	Gate          *GateSpec `json:"gate"`
	// Closure: "" (never), "any" (every masked value is promoted to the
	// value closure) or "random" (only values that look random and are at
	// least closure.minLen long).
	Closure string `json:"closure"`
}

// GateSpec restricts a rule to credential-like names with random-looking values.
type GateSpec struct {
	KeyGroup   int    `json:"keyGroup"`
	KeyName    string `json:"keyName"`    // "credential"
	ValueShape string `json:"valueShape"` // "random"
}

type compiledRule struct {
	spec    RuleSpec
	re      *regexp.Regexp
	secrets []int
}

// LoadRules parses the embedded rules.v1.json.
func LoadRules() (*RuleSet, error) { return ParseRules(embeddedRules) }

// ParseRules parses and validates a rule file.
func ParseRules(data []byte) (*RuleSet, error) {
	var rs RuleSet
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("parsing mask rules: %w", err)
	}
	if rs.Placeholder.Prefix != PlaceholderPrefix || rs.Placeholder.Suffix != PlaceholderSuffix ||
		rs.Placeholder.HexChars != fpHexChars {
		return nil, fmt.Errorf("mask rules %q: placeholder format differs from the compiled one", rs.ID)
	}
	seen := map[string]bool{}
	for _, r := range rs.Rules {
		if r.ID == "" || r.Pattern == "" || seen[r.ID] {
			return nil, fmt.Errorf("mask rules: bad or duplicate rule %q", r.ID)
		}
		seen[r.ID] = true
	}
	return &rs, nil
}

func (rs *RuleSet) compile() ([]*compiledRule, error) {
	out := make([]*compiledRule, 0, len(rs.Rules))
	for _, spec := range rs.Rules {
		pat := spec.Pattern
		if strings.Contains(spec.Flags, "i") {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("mask rule %s: %w", spec.ID, err)
		}
		secrets := spec.SecretGroups
		if len(secrets) == 0 {
			secrets = []int{spec.SecretGroup}
		}
		for _, g := range secrets {
			if g > re.NumSubexp() {
				return nil, fmt.Errorf("mask rule %s: secret group %d out of range", spec.ID, g)
			}
		}
		if spec.Gate != nil && spec.Gate.KeyGroup > re.NumSubexp() {
			return nil, fmt.Errorf("mask rule %s: gate key group out of range", spec.ID)
		}
		out = append(out, &compiledRule{spec: spec, re: re, secrets: secrets})
	}
	return out, nil
}
