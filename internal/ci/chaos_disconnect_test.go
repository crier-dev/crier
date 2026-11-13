package ci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChaosDisconnectEvidenceContract(t *testing.T) {
	script, err := filepath.Abs(repoPath(".qa", "chaos-probes", "chaos-disconnect.sh"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("available suite rejects empty evidence", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		fakeGo := filepath.Join(bin, "go")
		if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\necho 'ok fixture/test 0.001s'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(t.TempDir(), "disc.log")
		if err := os.WriteFile(log, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script, "--validate", repo, log)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "suite is available but disconnect evidence is empty") {
			t.Fatalf("validation = %q, %v; want empty-evidence refusal", out, err)
		}
	})

	t.Run("available suite records command and result", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		fakeGo := filepath.Join(bin, "go")
		if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\necho 'ok fixture/test 0.001s'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(t.TempDir(), "disc.log")
		cmd := exec.Command("bash", script, repo)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DISC_LOG="+log)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("runner output %q: %v", out, err)
		}
		evidence, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"COMMAND: ", "go test ./... -count=1 -timeout 60s -p 1", "ok fixture/test", "RESULT: PASS (exit 0)"} {
			if !strings.Contains(string(evidence), want) {
				t.Errorf("evidence %q does not contain %q", evidence, want)
			}
		}
	})

	t.Run("cut-injected unrelated failures are reported, not tolerated", func(t *testing.T) {
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		fakeGo := filepath.Join(bin, "go")
		fakeSuite := `#!/bin/sh
printf '%s\n' 'ok fixture/passing 0.001s'
printf '%s\n' '--- FAIL: TestUnrelatedAssertion (0.001s)'
printf '%s\n' '    unrelated assertion failed'
printf '%s\n' 'FAIL'
printf '%s\n' 'FAIL fixture/unrelated 0.002s'
printf '%s\n' '--- FAIL: TestSecondUnrelated (0.001s)'
printf '%s\n' '    second unrelated assertion failed'
printf '%s\n' 'FAIL'
printf '%s\n' 'FAIL fixture/another-unrelated 0.003s'
exit 1
`
		if err := os.WriteFile(fakeGo, []byte(fakeSuite), 0o700); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(t.TempDir(), "disc.log")
		cmd := exec.Command("bash", script, repo)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DISC_LOG="+log, "NETWORK_CUT_APPLIED=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("runner unexpectedly passed: %q", out)
		}
		evidence, readErr := os.ReadFile(log)
		if readErr != nil {
			t.Fatal(readErr)
		}
		text := string(evidence)
		for _, want := range []string{
			"FAILURE_SET: packages=fixture/unrelated,fixture/another-unrelated tests=TestUnrelatedAssertion,TestSecondUnrelated",
			"VERDICT: FAIL: unrelated failure(s) under network cut; packages=fixture/unrelated,fixture/another-unrelated tests=TestUnrelatedAssertion,TestSecondUnrelated",
			"RESULT: FAIL (exit 1)",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("evidence %q does not contain %q", text, want)
			}
		}
		if strings.Contains(text, "VERDICT: UNVERIFIED") {
			t.Errorf("unrelated failures were incorrectly tolerated: %q", text)
		}
		if strings.Contains(text, "passing package") {
			t.Errorf("passing package was used as failure reason: %q", text)
		}
	})

	t.Run("missing suite path is explicitly unverified", func(t *testing.T) {
		repo := t.TempDir()
		bin := t.TempDir()
		fakeGo := filepath.Join(bin, "go")
		if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(t.TempDir(), "disc.log")
		cmd := exec.Command("bash", script, repo)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "DISC_LOG="+log)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "UNVERIFIED: repository test path unavailable") {
			t.Fatalf("runner output %q, err %v; want explicit UNVERIFIED", out, err)
		}
		evidence, readErr := os.ReadFile(log)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(evidence), "UNVERIFIED: repository test path unavailable") {
			t.Fatalf("evidence %q lacks explicit UNVERIFIED", evidence)
		}
	})
}
