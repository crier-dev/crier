package main

// DF-CRIER-303 — the dagger run-record store's startup writability check.
//
// The P1 this pins: CR_DAGGER_URL set with no CR_DAGGER_STORE_DIR defaults the
// store to /var/lib/crier/dagger, which a non-root user cannot create, and the
// boot used to fail with
//
//	initialize dagger control: daggerctl jsonl store: create directory:
//	mkdir /var/lib/crier: permission denied
//
// — the first ANCESTOR that could not be created, and no variable name anywhere
// in the message (reproduced on the control host AND on a fresh bunker box).
// The check under test turns that into an error naming CR_DAGGER_STORE_DIR, the
// resolved path and the uid, and it also refuses a store directory that EXISTS
// but is read-only — a shape the old MkdirAll path accepted, deferring the
// failure to the first Append long after the process reported success.
//
// Three tests, three different classes of evidence: the check's own contract on
// a table of store shapes (TestDaggerStoreUnwritable), the OPERATOR-VISIBLE
// error on the real boot path in a child process
// (TestDaggerStoreUnwritableFailsBoot), and the writable direction booting and
// serving the surface (TestDaggerStoreWritableBoots).

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// assertStoreErrorNames is the shared contract every store-directory failure
// must satisfy: it names the variable that selects the directory, the resolved
// path, and the uid that cannot write it — and it names the variable whose being
// set is why a store is required at all. A message that omits any of those is
// exactly the defect this row exists for.
func assertStoreErrorNames(t *testing.T, err error, dir string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got a nil error, want a store-directory error naming %s and %q", daggerStoreEnv, dir)
	}
	msg := err.Error()
	for _, want := range []string{
		daggerStoreEnv,
		dir,
		fmt.Sprintf("uid %d", os.Geteuid()),
		daggerURLEnv,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the store error does not name %q:\n  %s", want, msg)
		}
	}
}

// assertNoWriteProbeLeftBehind proves the writability probe cleans up after
// itself: a check that leaves files in an operator's store directory is a new
// defect wearing the old one's clothes.
func assertNoWriteProbeLeftBehind(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the store directory %q: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".crier-dagger-writable-") {
			t.Errorf("the write probe %q was left behind in %q", e.Name(), dir)
		}
	}
}

// TestDaggerStoreUnwritable pins the check's contract on a table of store
// shapes. The failing arms use uid-INDEPENDENT inputs wherever the outcome must
// be deterministic, so a root runner cannot silently neuter them; the arms that
// genuinely need an unprivileged user record a SKIP naming the precondition
// rather than turning into a green.
func TestDaggerStoreUnwritable(t *testing.T) {
	t.Run("a missing directory under a regular file", func(t *testing.T) {
		// A path whose parent is a regular file can never be created — as any
		// uid. This arm is therefore the deterministic core of the check and is
		// what the child-process boot test drives too.
		blocker := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blocker, []byte("regular file"), 0o644); err != nil {
			t.Fatalf("create the blocking file: %v", err)
		}
		dir := filepath.Join(blocker, "dagger")

		err := ensureDaggerStoreDir(dir)
		assertStoreErrorNames(t, err, dir)
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("the error does not wrap the OS cause (want ENOTDIR): %v", err)
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Errorf("the check left %q on disk out of a regular file", dir)
		} else if !errors.Is(statErr, syscall.ENOTDIR) && !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("stat %q: %v, want a not-a-directory or not-exist error (nothing may be created there)", dir, statErr)
		}
	})

	t.Run("an existing read-only directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: a 0o500 directory is still writable, so the read-only arm cannot be driven here")
		}
		dir := filepath.Join(t.TempDir(), "dagger")
		if err := os.MkdirAll(dir, 0o500); err != nil {
			t.Fatalf("seed the read-only directory: %v", err)
		}
		// THE PREMISE, asserted: MkdirAll alone judges this directory fine.
		// That is the gap the row names — a creatability test is not a
		// writability test — so if this ever stops being true the arm is
		// vacuous and must go red rather than quietly pass.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("PREMISE BROKEN: MkdirAll already rejects the read-only directory (%v), so this arm no longer proves the creatability/writability gap", err)
		}

		err := ensureDaggerStoreDir(dir)
		assertStoreErrorNames(t, err, dir)
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("the error does not wrap the permission failure: %v", err)
		}
	})

	t.Run("the shipped default on a box that cannot write it", func(t *testing.T) {
		// The row's own repro: CR_DAGGER_URL set with CR_DAGGER_STORE_DIR unset
		// resolves the store here. Skipped where the path is already usable (a
		// root runner, or a host an operator has prepared) — recorded as a
		// skip, never as a pass.
		const shippedDefault = "/var/lib/crier/dagger"
		if os.Geteuid() == 0 {
			t.Skip("running as root: /var/lib is writable, so the shipped default cannot be driven here")
		}
		err := ensureDaggerStoreDir(shippedDefault)
		if err == nil {
			t.Skipf("%s is writable on this host, so the failing arm cannot be driven here", shippedDefault)
		}
		assertStoreErrorNames(t, err, shippedDefault)
	})

	t.Run("an existing writable directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := ensureDaggerStoreDir(dir); err != nil {
			t.Fatalf("ensureDaggerStoreDir(%q) = %v, want nil", dir, err)
		}
		assertNoWriteProbeLeftBehind(t, dir)
	})

	t.Run("a missing directory under a writable parent is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "crier-dagger")
		if err := ensureDaggerStoreDir(dir); err != nil {
			t.Fatalf("ensureDaggerStoreDir(%q) = %v, want nil", dir, err)
		}
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			t.Fatalf("the check did not leave %q as a directory (stat: %v, err %v)", dir, fi, err)
		}
		assertNoWriteProbeLeftBehind(t, dir)
	})

	t.Run("an empty value is refused", func(t *testing.T) {
		for _, dir := range []string{"", "   "} {
			err := ensureDaggerStoreDir(dir)
			if err == nil {
				t.Fatalf("ensureDaggerStoreDir(%q) = nil, want an error", dir)
			}
			if !strings.Contains(err.Error(), daggerStoreEnv) {
				t.Errorf("the error for %q does not name %s: %v", dir, daggerStoreEnv, err)
			}
		}
	})
}

// The child half of TestDaggerStoreUnwritableFailsBoot.
const (
	dswChildEnv    = "CRIER_DF303_BOOT_CHILD"
	dswChildRun    = "^TestDaggerStoreUnwritableFailsBoot$"
	dswChildMarker = "DF-303-CHILD-RUN-FAILED-THE-BOOT-AS-EXPECTED"
)

// TestDaggerStoreUnwritableFailsBoot drives the REAL boot path (run()) with
// CR_DAGGER_URL set and an unusable store directory, and asserts what the
// operator sees: exit code 1 and one ERROR line naming the variable, the path
// and the uid.
//
// A child process (the helper-process pattern this package already uses for the
// QA-CRIER-17 fixture) is what makes the operator-visible message assertable:
// the child's stderr IS the pipe this test reads, so the line asserted below is
// exactly the line a container log or a terminal shows — no in-process logger
// interception, and no re-implementation of the message under test.
//
// The path is made unusable by putting it under a REGULAR FILE, a property of
// the filesystem rather than of the uid, so unlike a chmod-based setup this arm
// cannot be silently neutered by a root runner.
func TestDaggerStoreUnwritableFailsBoot(t *testing.T) {
	if os.Getenv(dswChildEnv) == "1" {
		dswBootChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run="+dswChildRun, "-test.v", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), dswChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the child boot probe did not complete: %v\nchild output:\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, dswChildMarker) {
		t.Fatalf("the child exited 0 but never printed %q, so run() did not fail the way this fixture drives it:\n%s",
			dswChildMarker, text)
	}
	// The operator-visible line. A boot that failed EARLIER than the dagger
	// check — or that printed the old ancestor-path message — cannot satisfy all
	// four of these, which is what makes the assertion sharp rather than
	// merely true of any failure.
	for _, want := range []string{
		"initialize dagger control",
		daggerStoreEnv,
		"not writable by uid",
		daggerURLEnv,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the boot log does not contain %q:\n%s", want, text)
		}
	}
}

// dswBootChild is the child half: boot with an unusable store directory, prove
// run() failed, and say so on stdout for the parent.
func dswBootChild(t *testing.T) {
	t.Helper()

	// Deterministic environment: no DB, no auth, no pidfile, and every disk
	// root a temp directory, so the ONLY thing this boot can trip over is the
	// store directory under test.
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CR_PIDFILE", "")
	t.Setenv("CR_GUARD_ENABLED", "false")
	t.Setenv("CR_SESSION_LOG_ROOT", t.TempDir())
	t.Setenv("CR_GROUP_ROOT", t.TempDir())
	t.Setenv("CRIER_PORT", strconv.Itoa(freePort(t)))

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("regular file"), 0o644); err != nil {
		t.Fatalf("create the blocking file: %v", err)
	}
	storeDir := filepath.Join(blocker, "dagger")

	t.Setenv(daggerURLEnv, "http://127.0.0.1:1")
	t.Setenv(daggerStoreEnv, storeDir)
	t.Setenv("CR_DAGGER_POLL_S", "0")

	done := make(chan int, 1)
	go func() { done <- run(nil) }()

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("PREMISE BROKEN: run() = %d, want 1 — an unusable %s no longer fails the boot, so this fixture proves nothing",
				code, daggerStoreEnv)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("run() did not return within 30s: the boot neither failed nor answered, so this fixture proves nothing")
	}

	// stdout is the parent's evidence that the child reached this point; the
	// message itself is asserted from the child's own stderr, which the parent
	// reads through the pipe it created. The write error is explicitly
	// discarded — the child's stdout is a pipe the parent already reads, and a
	// failed print there is reported by the parent's marker assertion.
	_, _ = fmt.Fprintln(os.Stdout, dswChildMarker)
}

// TestDaggerStoreWritableBoots is the writable direction of the row's
// acceptance criteria: with a store directory this user can write, the boot
// completes and the OPT-IN dagger surface is live on the REAL router.
//
// The directory deliberately does not exist yet, so what this exercises is the
// check's create path plus the probes behind it — not just a pre-made
// directory being accepted.
func TestDaggerStoreWritableBoots(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crier-dagger-"+strconv.Itoa(os.Getpid()))
	baseURL := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
		"CR_DAGGER_URL":        "http://127.0.0.1:1",
		"CR_DAGGER_STORE_DIR":  dir,
		"CR_DAGGER_POLL_S":     "0",
	})

	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("the boot did not leave the configured store directory %q in place (stat: %v, err %v)", dir, fi, err)
	}

	// startTestServerWithEnv has already proven /health answers; this proves the
	// surface under test is LIVE and not merely absent: an unregistered path
	// answers the router's 404, while a create with no prompt reaches the dagger
	// handler and answers 400.
	code, body := postJSON(t, &http.Client{Timeout: 2 * time.Second}, baseURL+"/dagger/runs", "{}")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /dagger/runs with an empty request: status %d, want 400 (body %s) — a 404 would mean the dagger surface was never registered",
			code, body)
	}
}
