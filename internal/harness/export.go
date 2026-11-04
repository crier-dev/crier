package harness

import (
	"context"
	"time"
)

// This file is the small exported surface the concrete adapters share. It keeps
// the exec/probe plumbing in one place so two adapters cannot grow two
// different process-group or version-probe implementations.

// ExecSpec is one child process: an argv, a cwd and optional stdin. No shell.
type ExecSpec = execSpec

// ExecResult is the outcome of one child run.
type ExecResult = execResult

// RunCommand runs one child under timeout, killing its process group on
// expiry.
func RunCommand(ctx context.Context, spec ExecSpec, timeout time.Duration) ExecResult {
	return runCommand(ctx, spec, timeout)
}

// ProbeBinary resolves binary and runs `--version` under the standard probe
// budget. Features is copied into the returned Capability.
func ProbeBinary(ctx context.Context, binary string, features map[string]bool) (Capability, error) {
	return probeBinary(ctx, binary, features)
}

// LookupBinary resolves a configured binary to an executable path.
func LookupBinary(path string) (string, error) { return lookupBinary(path) }

// WorkdirError validates a delivery cwd: it must exist and be a directory.
func WorkdirError(dir string) error { return workdirError(dir) }
