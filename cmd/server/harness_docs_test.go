// harness_docs_test.go — DF-CRIER-501: the adapter-registry/doc drift gate.
//
// docs/harness-adapters.md states, in prose and in a capability table, which
// harness adapters this build serves. That claim had nothing executing it:
// docs/claims.yaml declares no claim whose doc is docs/harness-adapters.md, so
// the docsclaims gate never reads the file, and the only test naming adapter
// names (TestHarnessListReportsEveryAdapter) asserts the CLI's output, not the
// doc. An adapter could be added to the registry — or removed — and the doc
// would keep describing a build that no longer exists.
//
// This gate closes that hole: it enumerates the adapter names from the LIVE
// registry (build.Registry over the shipped defaults — the same constructor and
// the same Names() source `crier harness list` iterates), then requires every
// one of them to appear as a data row of the doc's capability table (`| Name |
// Available | Version | Features |`, the table the doc introduces with
// "`crier harness list` on this build"). The row must also carry a real boolean
// in its Available cell: a row that names an adapter but answers nothing is not
// the capability table's claim.
//
// The check is factored to take the doc TEXT, so the same code is driven against
// mutated copies on every run — a deleted row and a doc with no capability table
// at all must both be reported. That mutation arm is what keeps the gate
// falsifiable after this tick: the negative control cannot rot into a check that
// passes because it stopped looking.
//
// Hermetic: no network, no server, no adapter probe (the registry is inspected,
// never executed), no temp ports. The only I/O is one read of the doc.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/build"
)

// harnessAdapterDocsFile is the doc under gate, relative to the repo root.
const harnessAdapterDocsFile = "docs/harness-adapters.md"

// harnessAdapterRegistryNames returns the adapter names the live registry can
// report, built exactly the way `crier harness list` builds it: the shipped
// default config wired through build.Registry. DefaultConfig (not LoadConfig)
// is deliberate — reading the ambient CR_HARNESS_* environment would let a
// worker shell with an adapter block disabled shrink this gate silently.
func harnessAdapterRegistryNames(t *testing.T) []string {
	t.Helper()
	cfg := harness.DefaultConfig()
	cfg.Enabled = true
	reg := build.Registry(cfg)
	names := reg.Names()
	if len(names) == 0 {
		t.Fatal("the harness registry reports no adapters; the doc gate would be vacuous")
	}
	// Non-vacuity in the other direction: a registry that silently lost an
	// adapter block would let the doc keep a row the build cannot serve, so
	// the served set must still be the closed set the config accepts.
	served := make(map[string]bool, len(names))
	for _, name := range names {
		served[name] = true
	}
	for _, known := range harness.KnownAdapters {
		if !served[known] {
			t.Errorf("adapter %q is in harness.KnownAdapters but the registry does not register it; `crier harness list` would omit it and this gate would not require its doc row", known)
		}
	}
	if len(names) != len(harness.KnownAdapters) {
		t.Errorf("registry reports %d adapters (%s), harness.KnownAdapters holds %d — the served set and the accepted set disagree",
			len(names), strings.Join(names, ", "), len(harness.KnownAdapters))
	}
	return names
}

// harnessDocRow is one data row of the doc's capability table.
type harnessDocRow struct {
	name      string // first cell — the adapter id
	available string // the Available cell, verbatim
	line      int    // 1-based line number in the doc
	raw       string
}

// harnessDocsTableLine reports whether line is a markdown table row.
func harnessDocsTableLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

// splitHarnessDocsRow splits a markdown table row into trimmed cells.
func splitHarnessDocsRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// harnessDocsSeparator reports whether line is a table's `|---|---|` rule.
func harnessDocsSeparator(line string) bool {
	if !harnessDocsTableLine(line) {
		return false
	}
	cells := splitHarnessDocsRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if c == "" || strings.Trim(c, "-: ") != "" || !strings.Contains(c, "-") {
			return false
		}
	}
	return true
}

// parseHarnessDocsCapabilityTable finds the doc's capability table and returns
// its data rows plus the index of its Available column. The table is identified
// by its header, not by a section heading: the first markdown table whose first
// column is "Name" and which also carries an "Available" column. The doc's
// other table ("| Harness | Continue | Create |") does not match, so the
// verified-surfaces rows can never stand in for capability rows.
func parseHarnessDocsCapabilityTable(doc string) ([]harnessDocRow, int, error) {
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if !harnessDocsTableLine(line) {
			continue
		}
		header := splitHarnessDocsRow(line)
		if len(header) < 3 || !strings.EqualFold(header[0], "name") {
			continue
		}
		availIdx := -1
		for j, cell := range header {
			if strings.EqualFold(cell, "available") {
				availIdx = j
				break
			}
		}
		if availIdx < 0 {
			continue
		}
		if i+1 >= len(lines) || !harnessDocsSeparator(lines[i+1]) {
			return nil, 0, fmt.Errorf("capability table header at %s line %d is not followed by a separator row", harnessAdapterDocsFile, i+1)
		}
		var rows []harnessDocRow
		for j := i + 2; j < len(lines) && harnessDocsTableLine(lines[j]); j++ {
			cells := splitHarnessDocsRow(lines[j])
			if len(cells) <= availIdx {
				return nil, 0, fmt.Errorf("%s line %d has %d cells, fewer than the Available column (%d): %s",
					harnessAdapterDocsFile, j+1, len(cells), availIdx+1, lines[j])
			}
			rows = append(rows, harnessDocRow{
				name:      cells[0],
				available: cells[availIdx],
				line:      j + 1,
				raw:       lines[j],
			})
		}
		return rows, availIdx, nil
	}
	return nil, 0, fmt.Errorf("%s carries no capability table: no markdown table has a \"Name\" column and an \"Available\" column (the doc must print the `crier harness list` table)", harnessAdapterDocsFile)
}

// checkHarnessAdapterDocs is the gate itself: for every adapter name the
// registry can report, the doc's capability table must carry a row naming it
// with a boolean Available cell; and no row may name an adapter neither the
// registry nor the alias table can resolve. Every finding names the adapter and
// the doc, so a failure is fixable without reading this file.
func checkHarnessAdapterDocs(doc string, names []string) []string {
	rows, _, err := parseHarnessDocsCapabilityTable(doc)
	if err != nil {
		return []string{err.Error()}
	}
	byName := make(map[string]harnessDocRow, len(rows))
	present := make([]string, 0, len(rows))
	for _, row := range rows {
		byName[row.name] = row
		present = append(present, row.name)
	}
	var findings []string
	for _, name := range names {
		row, ok := byName[name]
		if !ok {
			findings = append(findings, fmt.Sprintf(
				"adapter %q is registered in the harness registry but has no capability-table row in %s (rows: %s) — `crier harness list` prints it, the doc does not",
				name, harnessAdapterDocsFile, strings.Join(present, ", ")))
			continue
		}
		if row.available != "true" && row.available != "false" {
			findings = append(findings, fmt.Sprintf(
				"adapter %q has a capability-table row at %s line %d whose Available cell is %q, want true or false (the column is the probe's verdict, not free prose): %s",
				name, harnessAdapterDocsFile, row.line, row.available, row.raw))
		}
	}
	for _, row := range rows {
		if len(names) == 0 {
			break
		}
		if _, ok := harness.CanonicalName(row.name); !ok {
			findings = append(findings, fmt.Sprintf(
				"capability-table row at %s line %d names %q, which no adapter and no alias serves (known: %s)",
				harnessAdapterDocsFile, row.line, row.name, strings.Join(harness.AcceptedNames(), ", ")))
		}
	}
	return findings
}

// TestHarnessAdapterDocsCoverRegistry is the DF-CRIER-501 gate: every adapter
// name the harness registry can report appears in docs/harness-adapters.md's
// capability table. The subtests drive the SAME checker against mutated copies
// of the shipped doc, so the gate proves on every run that it can still fail —
// a gate nobody has seen go red is not evidence.
func TestHarnessAdapterDocsCoverRegistry(t *testing.T) {
	repoRoot := resolveRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, harnessAdapterDocsFile))
	if err != nil {
		t.Fatalf("read %s from %s: %v", harnessAdapterDocsFile, repoRoot, err)
	}
	doc := string(raw)
	names := harnessAdapterRegistryNames(t)
	t.Logf("registry adapter names: %s", strings.Join(names, ", "))

	t.Run("every registered adapter has a capability-table row", func(t *testing.T) {
		findings := checkHarnessAdapterDocs(doc, names)
		for _, f := range findings {
			t.Error(f)
		}
		if len(findings) == 0 {
			t.Logf("%s covers every registered adapter", harnessAdapterDocsFile)
		}
	})

	t.Run("a dropped adapter row is reported", func(t *testing.T) {
		// Mutation arm: delete the row of the adapter the doc is likeliest to
		// lose from the shipped bytes and require the checker to name it.
		target := names[0]
		rows, _, err := parseHarnessDocsCapabilityTable(doc)
		if err != nil {
			t.Fatalf("parse shipped doc: %v", err)
		}
		lines := strings.Split(doc, "\n")
		removed := false
		for _, row := range rows {
			if row.name != target {
				continue
			}
			lines[row.line-1] = "" // blank the row, keep the line numbering
			removed = true
			break
		}
		if !removed {
			t.Fatalf("cannot run the mutation arm: no capability-table row for %q in the shipped doc", target)
		}
		mutated := strings.Join(lines, "\n")
		if findings := checkHarnessAdapterDocs(mutated, names); len(findings) == 0 {
			t.Errorf("removing the %q row left the gate silent — it does not require the row", target)
		} else if !strings.Contains(strings.Join(findings, "\n"), target) {
			t.Errorf("the gate flagged the mutation but never named %q: %v", target, findings)
		}
	})

	t.Run("a doc with no capability table is reported", func(t *testing.T) {
		// The pre-fix shape: prose and a verified-surfaces table, no
		// `crier harness list` table at all. The gate must refuse to pass it
		// rather than find nothing to check.
		surfacesOnly := "# Harness adapters\n\n| Harness | Continue | Create |\n|---|---|---|\n| hermes | a | b |\n"
		findings := checkHarnessAdapterDocs(surfacesOnly, names)
		if len(findings) == 0 {
			t.Fatal("a doc with no capability table passed the gate — the check is vacuous without one")
		}
		if !strings.Contains(strings.Join(findings, "\n"), "no capability table") {
			t.Errorf("the failure does not explain the missing capability table: %v", findings)
		}
	})
}
