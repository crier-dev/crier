package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Pattern is one deterministic prematch rule (spec §6.4). Names are stable;
// regexes are Go regexp literals, case-insensitive where the seed table
// says so.
type Pattern struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Class   string `json:"class"`
}

// SeedPatterns is the built-in prematch table (spec §6.4). Class
// identifiers are the §1.1 identifiers (instruction_injection, jailbreak,
// masquerade, structured_object).
var SeedPatterns = []Pattern{
	{Name: "ignore_previous", Pattern: `(?i)ignore\s+(all\s+)?(previous|prior|above)\s+instructions?`, Class: "instruction_injection"},
	{Name: "system_override", Pattern: `(?i)(you\s+are\s+now|from\s+now\s+on|act\s+as)\s+(dan|gpt[-\s]?\w*|an?\s+unrestricted|a\s+new\s+system)`, Class: "jailbreak"},
	{Name: "no_restrictions", Pattern: `(?i)(no\s+(rules|restrictions|filter)|unrestricted\s+mode|without\s+(any\s+)?(rules|limits))`, Class: "jailbreak"},
	{Name: "hidden_cot", Pattern: `(?i)(show\s+(your\s+)?(chain|steps?|reasoning)|think\s+step\s+by\s+step)`, Class: "jailbreak"},
	{Name: "system_role", Pattern: `"role"\s*:\s*"system"`, Class: "structured_object"},
	{Name: "control_keys", Pattern: `"(system|instructions|prompt|tools|schema)"\s*:`, Class: "structured_object"},
	{Name: "b64_blob", Pattern: `[A-Za-z0-9+/]{80,}={0,2}`, Class: "masquerade"},
	{Name: "stringified_json", Pattern: `"(\\u00[0-9a-f]{2}|\\")?[^"]*\\"\s*[:{]`, Class: "masquerade"},
	{Name: "disguised_prompt", Pattern: `(?i)(this\s+is\s+(not\s+)?(a\s+)?(prompt|instruction)|treat\s+as\s+(data|text))\s*:`, Class: "masquerade"},
	{Name: "ignore_above", Pattern: `(?i)ignore\s+everything\s+above`, Class: "instruction_injection"},
}

// PreScanner runs the deterministic prematch scan over raw payload bytes
// (spec §6.4): it enriches the prompt (<prematch> section) and is the only
// verdict source for over-cap payloads (§6.3).
type PreScanner struct {
	patterns []compiledPattern
}

type compiledPattern struct {
	name  string
	class string
	re    *regexp.Regexp
}

// NewPreScanner compiles the seed table plus CR_GUARD_PATTERNS_EXTRA (a
// JSON array of {"name","pattern","class"}; a duplicate name replaces the
// built-in). Invalid JSON or regex → error (the server fails fast rather
// than silently guarding with a partial table).
func NewPreScanner(extraJSON string) (*PreScanner, error) {
	patterns := append([]Pattern(nil), SeedPatterns...)
	if extraJSON != "" {
		var extra []Pattern
		if err := json.Unmarshal([]byte(extraJSON), &extra); err != nil {
			return nil, fmt.Errorf("guard: CR_GUARD_PATTERNS_EXTRA: %w", err)
		}
		for _, p := range extra {
			replaced := false
			for i := range patterns {
				if patterns[i].Name == p.Name {
					patterns[i] = p
					replaced = true
					break
				}
			}
			if !replaced {
				patterns = append(patterns, p)
			}
		}
	}
	sc := &PreScanner{patterns: make([]compiledPattern, 0, len(patterns))}
	for _, p := range patterns {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("guard: pattern %q: %w", p.Name, err)
		}
		sc.patterns = append(sc.patterns, compiledPattern{name: p.Name, class: p.Class, re: re})
	}
	return sc, nil
}

// Scan runs the table over raw bytes. Returns matched names in table
// order, deduplicated (spec §6.4).
func (s *PreScanner) Scan(payload []byte) []string {
	var out []string
	seen := make(map[string]bool, len(s.patterns))
	for _, p := range s.patterns {
		if p.re.Match(payload) && !seen[p.name] {
			seen[p.name] = true
			out = append(out, p.name)
		}
	}
	return out
}

// ClassOf returns the §1.1 attack-class identifier for a pattern name
// ("" when unknown).
func (s *PreScanner) ClassOf(name string) string {
	for _, p := range s.patterns {
		if p.name == name {
			return p.class
		}
	}
	return ""
}
