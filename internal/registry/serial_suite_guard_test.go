package registry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestIntegrationSuitesAreSerial pins the INT-CI-015 regression.
//
// Every integration-tagged suite in this module runs against ONE shared
// testcontainers Postgres per package TestMain, and the per-test helpers wipe
// that shared schema before each test (newTestStore's DROP ... CASCADE over
// agents, inbox_entries, schema_migrations, ...). A t.Parallel() call in an
// integration-tagged file therefore lets concurrent tests drop the schema out
// from under each other: the CI run 40318109287 / job 121558530579 failure was
// ~25 tests red with 42P01 relation-does-not-exist (schema_migrations, agents,
// inbox_entries) plus 40P01 deadlocks on the DDL locks — measured after the
// INT-CI-014 parallel-batch sweep (79081696) stamped 106 call sites across the
// integration-tagged files. The serial regime this guard pins was green at
// 1m42s (run 40180072531), well inside the 5m budget, so parallelism bought
// nothing here and cost the job.
//
// The file carries NO build tag on purpose: it must run in the default regime
// (gitreins Tier-1 pre-commit, make test, CI Build & Test) AND inside
// -tags=integration runs (the tag ADDS constraints, it never excludes untagged
// files), so a re-landed stamp is rejected at commit time — before it can
// redden the integration job again. The guard fails if a bare `t.Parallel()`
// statement appears in any //go:build integration file. It matches the bare
// statement line only (the exact shape the sweep stamped), so comments and
// string literals mentioning the call never false-positive.
func TestIntegrationSuitesAreSerial(t *testing.T) {
	root := repoRoot(t)
	bareParallel := regexp.MustCompile(`^\s*t\.Parallel\(\)\s*$`)
	var offenders []string

	skipDirs := map[string]bool{
		".git": true, "vendor": true, "bin": true,
		".vfs": true, "node_modules": true, "testdata": true,
	}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) == 0 {
			return nil
		}
		first := lines[0]
		if !strings.HasPrefix(first, "//go:build") || !strings.Contains(first, "integration") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		for i, ln := range lines {
			if bareParallel.MatchString(ln) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(ln)))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("integration tree scan failed under %s: %v", root, walkErr)
	}
	if len(offenders) > 0 {
		t.Fatalf("t.Parallel() re-landed in integration-tagged files — the shared testcontainers Postgres cannot survive concurrent schema drops (INT-CI-015); remove the stamps:\n%s",
			strings.Join(offenders, "\n"))
	}
}

// repoRoot locates the module root so the guard scans the whole tree no matter
// which package it is compiled into. runtime.Caller(0) is primary because it
// does not depend on the test process cwd; the package dir is the fallback.
// Both are verified against go.mod and a miss FAILS — a guard that cannot find
// the tree must stay red, never silently pass.
func repoRoot(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		// thisFile: <root>/internal/registry/<this file>
		candidates = append(candidates, filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Dir(filepath.Dir(cwd)))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(filepath.Join(c, "go.mod")); err == nil && !fi.IsDir() {
			return c
		}
	}
	t.Fatalf("cannot locate module root (tried %v): the serial-suite guard refuses to run unverified", candidates)
	return ""
}
