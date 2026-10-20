package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Behavior claims use an id-keyed allowlist, never commands from YAML. Running
// fixtures proves both refusal and acceptance without repairing/mutating the
// checkout that make docs-check is measuring (fresh CI jobs are hookless).
func liveBehavior(repoRoot, id string) (string, error) {
	if id != "BEHAVIOR-HOOK-DOCTOR" {
		return "", fmt.Errorf("no behavior probe configured for %s", id)
	}
	// DF-CRIER-433 raised this budget: the selftest's acceptance cases now RUN the
	// installed hooks in scratch repositories (a healthy board and a staged shrink
	// per fixture), so the fixture battery costs seconds, not milliseconds. The
	// bound stays generous relative to the measured wall clock (~20-30s on a
	// loaded box) — it exists to stop a hang, not to time the battery.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "doctor-hooks-selftest")
	cmd.Dir = repoRoot
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("doctor-hooks-selftest: %w\n%s", err, out)
	}
	// The evidence the claim stands on: the refusals, the marker NEUTER control,
	// and (DF-CRIER-433) the EXECUTION probe actually running and its own NEUTER
	// control accepting a fixture the real doctor refuses.
	for _, want := range []string{
		"doctor-hooks selftest: PASS (",
		"NEUTER accepts unset fixture rejected by real doctor: exit 0",
		"NEUTER probe verdict accepts what the real doctor rejects: exit 0",
		"doctor-hooks: probe pre-commit: EXECUTED",
		"unconditional `exit 0` above the board arm (pre-commit): exit 1",
		"probe-only disarm (guard the static arm cannot see): exit 1",
	} {
		if !strings.Contains(string(out), want) {
			return "", fmt.Errorf("doctor-hooks-selftest did not report its behavior/NEUTER/probe evidence (%q):\n%s", want, out)
		}
	}
	return "doctor-hooks-selftest", nil
}

func verifyBehaviorClaims(set claimsFile, probe func(string) (string, error)) []claimResult {
	var results []claimResult
	for _, c := range set.Claims {
		if c.Kind != "behavior" {
			continue
		}
		got, err := probe(c.ID)
		if err != nil || fmt.Sprint(c.Expect) != got {
			results = append(results, claimResult{claim: c, kind: "behavior", msg: fmt.Sprintf(
				"claim %s [behavior]: doc=%s quote=%q expected=%v observed=%q error=%v",
				c.ID, c.Doc, c.Quote, c.Expect, got, err)})
		}
	}
	return results
}

func TestDocsClaimsBehaviors(t *testing.T) {
	root := resolveRepoRoot(t)
	set := loadClaimsFile(t, root)
	// Removing or relabeling the claim must not make the behavioral gate vacuous.
	found := 0
	for _, c := range set.Claims {
		if c.ID == "BEHAVIOR-HOOK-DOCTOR" && c.Kind == "behavior" && c.XFail == nil {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one non-xfail BEHAVIOR-HOOK-DOCTOR behavior claim, got %d", found)
	}
	reportFindings(t, verifyBehaviorClaims(set, func(id string) (string, error) {
		return liveBehavior(root, id)
	}))
}

func TestDocsClaimsBehaviorNegativeControl(t *testing.T) {
	set := claimsFile{Claims: []docClaim{{ID: "BEHAVIOR-HOOK-DOCTOR", Kind: "behavior", Doc: "README.md", Quote: "doctor rejects", Expect: "doctor-hooks-selftest"}}}
	for _, tc := range []struct {
		name string
		got  string
		err  error
		fail bool
	}{
		{"accept", "doctor-hooks-selftest", nil, false},
		{"wrong evidence", "", nil, true},
		{"failing command", "doctor-hooks-selftest", errors.New("exit status 1"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := verifyBehaviorClaims(set, func(string) (string, error) { return tc.got, tc.err })
			if (len(res) > 0) != tc.fail {
				t.Fatalf("findings=%v, want failure=%v", res, tc.fail)
			}
			if tc.fail && !strings.Contains(res[0].msg, "BEHAVIOR-HOOK-DOCTOR") {
				t.Fatalf("failure must name the claim: %v", res)
			}
		})
	}
	if _, err := liveBehavior(t.TempDir(), "UNKNOWN"); err == nil {
		t.Fatal("unknown behavior claim accepted")
	}
	if _, err := liveBehavior(t.TempDir(), "BEHAVIOR-HOOK-DOCTOR"); err == nil {
		t.Fatal("missing behavior target accepted")
	}
}
