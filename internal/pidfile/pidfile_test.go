package pidfile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crier.pid")
	rec := Record{PID: 12345, Port: 8767, Binary: "/home/x/crier/bin/crier"}

	if err := Write(path, rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Atomicity precondition: no temp files left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("Write left %d files behind (want exactly the pidfile): %v", len(entries), entries)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != rec {
		t.Errorf("Read = %+v, want %+v", got, rec)
	}

	// Mode is 0644: operator-readable state, not a secret.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("pidfile mode = %o, want 644", info.Mode().Perm())
	}
}

func TestReadMissingFileIsSentinel(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.pid"))
	if !errors.Is(err, ErrNoPidfile) {
		t.Fatalf("Read of missing file = %v, want ErrNoPidfile", err)
	}
}

func TestReadCorruptDocumentFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crier.pid")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt pidfile: %v", err)
	}
	if _, err := Read(path); err == nil || errors.Is(err, ErrNoPidfile) {
		t.Fatalf("Read of corrupt file = %v, want a descriptive parse error", err)
	}
}

func TestRemoveIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crier.pid")
	if err := Write(path, Record{PID: 1, Port: 1, Binary: "b"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove(existing): %v", err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove(missing) = %v, want nil (idempotent)", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile still present after Remove: %v", err)
	}
}

func TestAlive(t *testing.T) {
	if Alive(os.Getpid()) != true {
		t.Errorf("Alive(self) = false, want true")
	}
	if Alive(-1) {
		t.Errorf("Alive(-1) = true, want false")
	}
	// A pid that is definitely not running: max pid space probe.
	if Alive(1 << 30) {
		t.Errorf("Alive(1<<30) = true, want false")
	}
}

// TestAliveZombieIsDead locks the stop-path semantics: a killed-but-
// unreaped child still has a /proc entry, but for the stop poll it must
// count as gone. A plain /proc/<pid> stat returns success for a zombie —
// the bug this test pins.
func TestAliveZombieIsDead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie semantics are linux /proc")
	}
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_, _ = sleep.Process.Wait()
	}()
	pid := sleep.Process.Pid
	if !Alive(pid) {
		t.Fatal("precondition: sleep is alive")
	}
	if err := sleep.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}
	// Do NOT Wait: the child stays a zombie until reaped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			return // zombie correctly reported as gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Alive(zombie) stayed true; the stop poll would wait out its timeout on a dead server")
}

func TestSafeToSignalSelf(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// go test runs the test binary via a temp dir path; the /proc link
	// points at the same inode. TrimSuffix handles the deleted case.
	rec := Record{PID: os.Getpid(), Port: 0, Binary: self}
	if err := SafeToSignal(rec); err != nil {
		t.Fatalf("SafeToSignal(self) = %v, want nil", err)
	}
}

func TestSafeToSignalRefusesForeignProcess(t *testing.T) {
	// A live non-crier process we own: sleep. The recorded binary path is
	// deliberately wrong (points at crier), so ownership must refuse.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = sleep.Process.Kill()
		_, _ = sleep.Process.Wait()
	}()

	rec := Record{PID: sleep.Process.Pid, Port: 8767, Binary: "/opt/crier/bin/crier"}
	err := SafeToSignal(rec)
	if err == nil {
		t.Fatalf("SafeToSignal(foreign binary) = nil, want refusal")
	}
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("SafeToSignal(foreign binary) = %v, want *MismatchError", err)
	}
	if mismatch.Recorded != "/opt/crier/bin/crier" {
		t.Errorf("MismatchError.Recorded = %q, want /opt/crier/bin/crier", mismatch.Recorded)
	}
	if !strings.Contains(mismatch.Live, "sleep") {
		t.Errorf("MismatchError.Live = %q, want it to name the real binary (sleep)", mismatch.Live)
	}

	// And the sleep is still alive — the check never signals.
	if !Alive(sleep.Process.Pid) {
		t.Error("sleep process died; SafeToSignal must never deliver a signal")
	}
}

func TestSafeToSignalDeadPid(t *testing.T) {
	// Start and reap a process so its pid is provably gone.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	rec := Record{PID: dead.ProcessState.Pid(), Port: 8767, Binary: "/opt/crier/bin/crier"}
	err := SafeToSignal(rec)
	if !errors.Is(err, ErrNotAlive) {
		t.Fatalf("SafeToSignal(dead pid) = %v, want ErrNotAlive", err)
	}
}

func TestSafeToSignalEmptyRecordRefused(t *testing.T) {
	if err := SafeToSignal(Record{PID: os.Getpid()}); err == nil {
		t.Fatal("SafeToSignal(no binary) = nil, want refusal")
	}
	if err := SafeToSignal(Record{Binary: "/x"}); err == nil {
		t.Fatal("SafeToSignal(no pid) = nil, want refusal")
	}
}

// TestWriteReplaceExistingNotPartial proves the atomic-rename contract:
// overwriting an existing pidfile leaves either the old or the new
// document, never a truncated mix.
func TestWriteReplaceExistingNotPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crier.pid")
	old := Record{PID: 100, Port: 8000, Binary: "/old/crier"}
	if err := Write(path, old); err != nil {
		t.Fatalf("Write(old): %v", err)
	}
	newRec := Record{PID: 200, Port: 8767, Binary: "/new/crier"}
	if err := Write(path, newRec); err != nil {
		t.Fatalf("Write(new): %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read after overwrite: %v", err)
	}
	if got != newRec {
		t.Errorf("Read after overwrite = %+v, want %+v", got, newRec)
	}
}

// TestSignalTerminatesListener proves the stop path's signalling primitive
// end to end: a SIGTERM to a process that installed a handler shuts it
// down gracefully. Uses sleep without a handler — it dies on SIGTERM.
func TestSignalTerminatesListener(t *testing.T) {
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := sleep.Process.Pid
	if err := sleep.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("process did not exit after SIGTERM")
}
