// Package testsupport holds test-only helpers shared by crier's packages.
//
// # Why this package exists (DF-CRIER-253)
//
// Several entrypoint tests build the command under test with `go build` and
// then assert on the resulting binary's build identity. While that build ran
// with the live working directory as its Cmd.Dir, ANY concurrent writer in the
// package directory — a sibling worker mid-edit, a half-applied patch, an
// editor buffer saved mid-keystroke — made the package fail to COMPILE, and
// the test reported that as its own failure:
//
//	cmd/server/zz_sibling_probe.go:3:22: syntax error: unexpected {, expected )
//	FAIL github.com/crier-dev/crier/cmd/server [build failed]
//
// That is ambient metadata, not a defect in the code under test, and it has
// already burned real evidence: a tier-2 judge run on an unrelated task
// recorded exactly that FAIL. SnapshotBuildDir closes the hole — the build
// runs against an isolated checkout of HEAD, so a sibling's in-flight edit
// cannot red the package.
//
// This package imports "testing" and is therefore TEST-ONLY. It is never
// imported by production code or by a main package; it exists as its own
// package so the entrypoint tests share one implementation (and one
// documented override) instead of drifting copies, and so the isolation rule
// is stated in exactly one place.
package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SnapshotDirEnv names the environment variable a test can set to use a
// directory of its own as the snapshot instead of checking out HEAD.
//
// It exists for NEGATIVE CONTROLS: a test that must prove the identity
// assertions still BITE (i.e. that a wrong build identity FAILS) can aim the
// entrypoint tests at a deliberately sabotaged tree — a checkout of HEAD with
// the version rendering broken — without editing the live repository.
//
// Set-but-empty is treated as unset, so a stray `CRIER_TEST_SNAPSHOT_DIR=` in
// an environment cannot silently turn the isolation off.
const SnapshotDirEnv = "CRIER_TEST_SNAPSHOT_DIR"

// SnapshotBuildDir returns the directory a test must run its package build
// from: a checkout of the repository's current HEAD that is isolated from the
// live working tree, with pkgDir's package at the same repo-relative path.
//
// The snapshot necessarily keeps git metadata. The Go toolchain stamps VCS
// metadata (vcs.revision / vcs.time / vcs.modified) into any main package it
// builds inside a git checkout, and internal/buildinfo reads that stamp as its
// fallback identity. A plain file copy or a `git archive` extract in a temp
// dir therefore LOSES the commit and makes the identity tests fail for the
// wrong reason — a measured sibling finding of this tick. The snapshot is made
// with
//
//	git clone --shared --no-checkout <repo> <tmp>
//	git -C <tmp> checkout --detach <HEAD sha>
//
// which leaves the live .git untouched (no `git worktree` admin entries to
// leak or prune) and materializes HEAD, not the working tree.
//
// The snapshot is removed by a t.Cleanup hook, so it leaves no residue in the
// temp dir and no git admin entry behind.
//
// Fallbacks, in order:
//
//  1. When SnapshotDirEnv is set and non-empty, that directory is used as-is
//     (the negative-control override). Nothing is cloned and nothing is
//     cleaned up — the directory belongs to the caller.
//  2. When the snapshot cannot be created — git missing, not a repository,
//     HEAD unresolvable — the LIVE package directory is returned and the
//     explicit reason is logged, so a build outside a git checkout degrades to
//     the pre-DF-CRIER-253 behaviour instead of hard-failing.
func SnapshotBuildDir(t *testing.T, pkgDir string) string {
	t.Helper()

	absPkgDir, err := filepath.Abs(pkgDir)
	if err != nil {
		return fallback(t, pkgDir, "cannot resolve %q: %v", pkgDir, err)
	}

	repoRoot, err := git(absPkgDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fallback(t, pkgDir, "%s is not inside a git work tree (%v): git metadata cannot be snapshotted", absPkgDir, err)
	}
	rel, err := filepath.Rel(repoRoot, absPkgDir)
	if err != nil {
		return fallback(t, pkgDir, "cannot make %q relative to the repo root %q: %v", absPkgDir, repoRoot, err)
	}
	rel = filepath.ToSlash(rel)

	root, err := snapshotRoot(t, repoRoot, absPkgDir, rel)
	if err != nil {
		return fallback(t, pkgDir, "%v", err)
	}

	buildDir := filepath.Join(root, filepath.FromSlash(rel))
	if info, statErr := os.Stat(buildDir); statErr != nil || !info.IsDir() {
		return fallback(t, pkgDir, "snapshot %s has no package directory %s", root, buildDir)
	}
	t.Logf("DF-CRIER-253: building %s from the isolated HEAD snapshot %s", rel, root)
	return buildDir
}

// fallback reports why no snapshot was available and returns the live package
// directory, which is the pre-DF-CRIER-253 behaviour: the build is not
// isolated, but the test still runs instead of hard-failing on missing git.
func fallback(t *testing.T, pkgDir, format string, args ...any) string {
	t.Helper()
	t.Logf("DF-CRIER-253: no isolated HEAD snapshot (%s) — building in the live directory %s; a concurrent sibling edit can still red this package", fmt.Sprintf(format, args...), pkgDir)
	return pkgDir
}

// snapshotRoot returns the root of the tree the build must run in, creating the
// clone and registering its cleanup when it makes one.
func snapshotRoot(t *testing.T, repoRoot, absPkgDir, rel string) (string, error) {
	t.Helper()

	if override := strings.TrimSpace(os.Getenv(SnapshotDirEnv)); override != "" {
		abs, err := filepath.Abs(override)
		if err == nil {
			if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
				t.Logf("DF-CRIER-253: using %s=%s as the snapshot (override; no HEAD checkout was made)", SnapshotDirEnv, abs)
				return abs, nil
			}
		}
		t.Logf("%s=%q is not a usable directory — ignoring the override and snapshotting HEAD instead", SnapshotDirEnv, override)
	}

	head, err := git(absPkgDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("cannot resolve HEAD: %v", err)
	}

	dir, err := os.MkdirTemp("", "crier-head-snapshot-")
	if err != nil {
		return "", fmt.Errorf("cannot create the snapshot directory: %v", err)
	}
	// Remove the snapshot whether the run passed, failed or panicked. A
	// `git clone --shared` borrows the source repo's object store; deleting
	// this directory cannot damage the live .git (git never mutates an
	// alternate's objects).
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	snapshot := filepath.Join(dir, "snapshot")
	if _, err := git("", "clone", "--shared", "--no-checkout", repoRoot, snapshot); err != nil {
		return "", fmt.Errorf("cannot clone %s: %v", repoRoot, err)
	}
	// Detached HEAD at the revision observed above, not at whatever branch the
	// clone happened to leave checked out.
	if _, err := git(snapshot, "checkout", "--detach", head); err != nil {
		return "", fmt.Errorf("cannot check out %s in the snapshot: %v", head, err)
	}
	if info, err := os.Stat(filepath.Join(snapshot, filepath.FromSlash(rel))); err != nil || !info.IsDir() {
		return "", fmt.Errorf("snapshot %s has no package directory %s", snapshot, rel)
	}
	return snapshot, nil
}

// git runs a git command, optionally in dir (empty = the process working
// directory), and returns its trimmed stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
