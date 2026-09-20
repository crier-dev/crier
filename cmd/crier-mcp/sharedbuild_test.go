package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/crier-dev/crier/internal/testsupport"
)

// CI-016 — bound the test suite's host load. This package's exec-based tests
// (TestMCPServerInitialize, TestMCPServerCLIFlags, and the three run()-wiring
// tests in registration_test.go) each used to take their OWN isolated snapshot
// of the tree and run their OWN `go build` of the identical package: five
// `git clone --shared` plus five `go build` subprocesses per
// `go test ./cmd/crier-mcp` run (measured with `strace -f -e execve` at the
// base of this change). None of those tests mutates the binary or its layout —
// each only execs it with different env and flags — so ONE session-shared
// build covers all five, and no per-test private copy is required.
//
// The DF-CRIER-253/259 guarantees are unchanged: the build still runs from
// the isolated snapshot (committed content only, git metadata always present),
// and the HEAD the identity assertions compare against is still the
// SNAPSHOT's own revision, captured while the snapshot is alive.
//
// The snapshot's t.Cleanup stays bound to whichever test triggers the build:
// the snapshot is needed only for the build and the HEAD capture, both of
// which complete inside once.Do, and the finished binary does not depend on
// the directory it was built in. A future test that needs the snapshot
// DIRECTORY itself (not just the binary) after the triggering test has ended
// must take its own testsupport.SnapshotBuildDir instead of extending this
// helper.
type mcpSharedState struct {
	once sync.Once
	dir  string // session-scoped temp dir holding the shared binary
	bin  string // the shared crier-mcp binary
	head string // the snapshot's short HEAD at build time ("" when unresolvable)
	err  error  // the build failure every caller reports verbatim
}

var mcpShared mcpSharedState

// TestMain owns the session-scoped temp dir: no single test outlives every
// consumer of the binary, so process exit is the only correct cleanup point.
func TestMain(m *testing.M) {
	code := m.Run()
	if mcpShared.dir != "" {
		_ = os.RemoveAll(mcpShared.dir)
	}
	os.Exit(code)
}

// mcpSharedTarget returns the package binary built once per test session,
// plus the short HEAD of the snapshot it was built from ("" when no revision
// could be resolved — the identity assertions report that loudly themselves).
func mcpSharedTarget(t *testing.T) (bin, snapshotHead string) {
	t.Helper()
	mcpShared.once.Do(func() {
		dir, err := os.MkdirTemp("", "crier-mcp-session-")
		if err != nil {
			mcpShared.err = fmt.Errorf("cannot create the session binary dir: %w", err)
			return
		}
		mcpShared.dir = dir

		// ONE snapshot per session, not one per test (CI-016). The
		// DF-CRIER-260 rationale that used to sit at every call site still
		// applies to this build: it compiles committed content only, so a
		// sibling worker mid-edit cannot red it.
		buildDir := testsupport.SnapshotBuildDir(t, ".")
		mcpShared.head = gitShortHead(t, buildDir)
		t.Logf("CI-016: session-shared crier-mcp build from snapshot %s (HEAD %q)", buildDir, mcpShared.head)

		mcpShared.bin = filepath.Join(dir, "crier-mcp")
		build := exec.Command("go", "build", "-o", mcpShared.bin, ".")
		build.Dir = buildDir
		if out, err := build.CombinedOutput(); err != nil {
			mcpShared.err = fmt.Errorf("build crier-mcp from %s: %v\n%s", buildDir, err, out)
			return
		}
	})
	if mcpShared.err != nil {
		t.Fatalf("shared crier-mcp build (CI-016 session binary): %v", mcpShared.err)
	}
	return mcpShared.bin, mcpShared.head
}

// TestMCPSharedTargetIsStable pins the sharing contract: every caller in this
// package gets the SAME binary path, and that path is an existing executable
// file. If a future change makes a caller rebuild the package or point at a
// per-test copy, this fails before the load regression can land silently.
func TestMCPSharedTargetIsStable(t *testing.T) {
	first, head := mcpSharedTarget(t)
	second, headAgain := mcpSharedTarget(t)
	if first != second {
		t.Fatalf("two mcpSharedTarget calls returned different binaries %q and %q — the session build must be shared, not per-test", first, second)
	}
	if head != headAgain {
		t.Fatalf("snapshot HEAD flipped from %q to %q within one session", head, headAgain)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("the shared binary %s does not exist: %v", first, err)
	}
	if info.IsDir() {
		t.Fatalf("the shared binary %s is a directory", first)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("the shared binary %s is not executable (mode %s)", first, info.Mode())
	}
}

// TestOnePackageBuildPerSession is the CI-016 source invariant: across this
// package's test files there is exactly ONE `go build` of the package and ONE
// testsupport.SnapshotBuildDir call, both in sharedbuild_test.go. The
// behaviour it pins was measured with `strace -f -e execve` — five `go build`
// + five `git clone` per suite run before this change, one of each after —
// but strace is not a gate, so the pattern count is asserted on source the
// same way docsclaims_test.go pins the Makefile's stamping. A test that
// reintroduces its own build (the load regression this change closes) fails
// here even when its behaviour is otherwise identical.
func TestOnePackageBuildPerSession(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's Go files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found — the invariant is being checked from outside the package directory")
	}

	goBuildRE := regexp.MustCompile(`exec\.Command\("go", "build"`)
	snapshotRE := regexp.MustCompile(`testsupport\.SnapshotBuildDir`)

	var builders, snapshotters []string
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(raw)
		if goBuildRE.MatchString(text) {
			builders = append(builders, name)
		}
		if snapshotRE.MatchString(text) {
			snapshotters = append(snapshotters, name)
		}
	}

	want := []string{"sharedbuild_test.go"}
	if !slicesEqual(builders, want) {
		t.Errorf("`go build` of the package appears in %v — it must exist ONLY in sharedbuild_test.go (the CI-016 session-shared build); every other test must consume mcpSharedTarget", builders)
	}
	if !slicesEqual(snapshotters, want) {
		t.Errorf("testsupport.SnapshotBuildDir appears in %v — the snapshot must be taken ONLY by the shared build in sharedbuild_test.go, once per session", snapshotters)
	}
}

// slicesEqual reports whether two string slices are equal, treating nil and
// empty as equal (filepath.Glob returned nothing vs. a filter that matched
// nothing mean the same thing here).
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
