// Package pidfile gives the crier server an operator-facing lifecycle: a
// small JSON record written after the listener binds, and a fail-closed
// ownership check so the -stop path can never signal a process it did not
// start (DF-CRIER-194).
package pidfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoPidfile reports that no pidfile exists at the requested path. The
// stop path treats this as "nothing to stop" — a success, not a failure —
// so it must stay distinguishable from every other read error.
var ErrNoPidfile = errors.New("no pidfile")

// ErrNotAlive reports that the recorded pid is not running (no /proc
// entry). The stop path treats this as a stale pidfile: remove it and
// succeed.
var ErrNotAlive = errors.New("pid not running")

// MismatchError is returned by SafeToSignal when the recorded pid IS alive
// but its /proc/<pid>/exe does not match the recorded binary path. The
// stop path refuses to signal in this case; carrying both paths lets the
// refusal message show the operator exactly what changed.
type MismatchError struct {
	Recorded string
	Live     string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("pid runs a different binary: pidfile recorded %q but /proc exe is %q", e.Recorded, e.Live)
}

// Record is the pidfile document. Binary is the absolute path of the
// running executable (os.Executable at write time) — the value the
// ownership check compares against readlink /proc/<pid>/exe.
type Record struct {
	PID    int    `json:"pid"`
	Port   int    `json:"port"`
	Binary string `json:"binary"`
}

// Write stores rec at path atomically: the JSON is written to a temp file
// in the same directory and renamed over path, so a reader can never see a
// partial document. Mode is 0644 — the file is operator-readable state,
// not a secret.
func Write(path string, rec Record) error {
	if path == "" {
		return errors.New("pidfile: empty path")
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("pidfile: marshal record: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".crier-pid-*")
	if err != nil {
		return fmt.Errorf("pidfile: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// No-op after a successful rename (the name no longer exists).
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("pidfile: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("pidfile: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pidfile: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("pidfile: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// Read loads the Record at path. A missing file yields ErrNoPidfile; a
// present-but-invalid document yields a descriptive error (the stop path
// refuses rather than guessing on a corrupt record).
func Read(path string) (Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, ErrNoPidfile
		}
		return Record{}, fmt.Errorf("pidfile: read %s: %w", path, err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("pidfile: parse %s: %w", path, err)
	}
	if rec.PID <= 0 {
		return Record{}, fmt.Errorf("pidfile: parse %s: record has no pid", path)
	}
	return rec, nil
}

// Remove deletes the pidfile at path. A missing file is not an error —
// removal is idempotent, matching the stop path's "already gone" outcome.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pidfile: remove %s: %w", path, err)
	}
	return nil
}

// Alive reports whether pid currently exists AND is not a zombie. A
// killed-but-unreaped process keeps its /proc entry in state 'Z'; for the
// stop path that is "gone" (it holds no port and accepts no signal), so
// Alive must not report it as running or the stop poll waits out its full
// timeout on a dead server.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// The state field is the token after the parenthesised comm. A pid
	// can vanish between the two reads; treat that as not alive.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+1 < len(b) {
		state := b[i+1]
		if len(b) > i+2 {
			state = b[i+2]
		}
		return state != 'Z'
	}
	return false
}

// SafeToSignal decides whether the process named by rec may be signalled
// by the stop path. It FAILS CLOSED: an empty record, a dead pid, an
// unreadable /proc entry, or any difference between the recorded binary
// path and readlink /proc/<pid>/exe returns an error, and the caller must
// refuse to signal. Only the exact-match case returns nil.
//
// A " (deleted)" suffix on the live exe link (the binary was rebuilt after
// the server started — the running process still holds the old inode at
// the same path) is stripped before comparing: the path still identifies
// the crier this pidfile was written for.
func SafeToSignal(rec Record) error {
	if rec.PID <= 0 || rec.Binary == "" {
		return errors.New("pidfile: record is empty or has no binary path — refusing")
	}
	live, err := procExe(rec.PID)
	if errors.Is(err, ErrNotAlive) {
		return ErrNotAlive
	}
	if err != nil {
		return fmt.Errorf("pidfile: cannot verify pid %d — refusing: %w", rec.PID, err)
	}
	if live != rec.Binary {
		return &MismatchError{Recorded: rec.Binary, Live: live}
	}
	return nil
}
