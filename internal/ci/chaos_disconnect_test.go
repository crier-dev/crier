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
