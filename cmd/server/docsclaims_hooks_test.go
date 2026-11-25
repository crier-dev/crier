package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// per fixture), so the fixture battery costs seconds, not milliseconds.
	// DF-CRIER-438 raised it again, and by construction rather than by guesswork: the
	// probe drives a REAL Git entrypoint per hook per scenario (six fixture
	// repositories, each with its own `git init`/config/history, plus a real merge and
	// a real `git am`), and the selftest runs the doctor once per case — measured at
	// ~50s wall clock on an idle box (and ~80s while a case that has since been
	// folded into a premise measurement was still in the battery) before this
	// constant was changed.
	// The bound stays an anti-HANG bound, not a performance assertion: it must clear
	// the measured battery on a loaded CI runner, where git subprocesses are slower
	// than local ones, and still fail in minutes rather than never.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "doctor-hooks-selftest")
	cmd.Dir = repoRoot
	// DOCTOR_HOOKS_CLAIM_FAST selects the claim-fast battery (a few live checks
	// instead of the standalone 51-case battery, which keeps its own explicit
	// short bound for the intentional hanging-hook case). DF-CRIER-517: the probe
	// bound stays at the doctor's SHIPPED default (20s, scripts/doctor-hooks.sh)
	// — deliberately NOT overridden here. INT-CI-014 pinned
	// DOCTOR_PROBE_TIMEOUT=5 to protect the 60s package budget, but the bound is
	// a ceiling per hook invocation, not a cost: a healthy commit takes what it
	// takes (DF-CRIER-431 measured 0.7-1.0s through the full gate on the dev
	// host), so the pin saved nothing on green runs and made the claim STRICTER
	// than the product it documents — a real gated commit slower than 5s
	// (bunker-las-03: a fresh machine with cold caches, 5-7x the dev host) sits
	// inside the doctor's own 20s bound, yet the claim refused it. Measured at
	// the default bound the claim-fast battery is ~7s wall on a loaded 16-core
	// box. Worst case — a genuinely hung hook — the battery's <=10 boundable
	// invocations (4 under the real doctor, 6 under the NEUTER doctor) at 20s
	// each are still caught red, fail-closed by the DF-CRIER-441 refusal (exit
	// 124/137 never passes), with the 600s context below and make test's
	// -timeout 60s package alarm as outer backstops.
	cmd.Env = append(os.Environ(), "DOCTOR_HOOKS_CLAIM_FAST=1")
	cmd.WaitDelay = 2 * time.Second
	// The selftest's hook probes spawn bounded subprocess trees. Capture to a
	// file rather than CombinedOutput's pipe: a descendant that outlives a
	// timed-out probe must not keep the test blocked on an inherited pipe fd.
	logFile, err := os.CreateTemp("", "crier-doctor-hooks-*.log")
	if err != nil {
		return "", fmt.Errorf("doctor-hooks-selftest log: %w", err)
	}
	logPath := logFile.Name()
	defer func() { _ = os.Remove(logPath) }()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	err = cmd.Run()
	_ = logFile.Close()
	outBytes, readErr := os.ReadFile(logPath)
	if readErr != nil {
		return "", fmt.Errorf("doctor-hooks-selftest log read: %w", readErr)
	}
	out := outBytes
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
	// The probe shells out to `make doctor-hooks-selftest` (make + python + git
	// subprocess trees). INT-CI-014 ran this test serially because, queued at the
	// tail of the parallel batch, its subprocesses started after the drain under
	// post-storm cache pressure — measured 18s past a 60s package alarm (make2,
	// three-run floor). DF-CRIER-506 returned it to parallelBoot (the battery is
	// now the claim-fast subset — see liveBehavior for its budget).
	parallelBoot(t)
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
	parallelBoot(t) // subprocess/snapshot boot: parent-side env-free (INT-CI-014)
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
