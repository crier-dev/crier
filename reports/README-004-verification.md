# README-004 verification

This record closes the repository-local evidence gap for row `README-004`. No specification owns this documentation-alignment task (`spec=none:no-spec-owns-this`).

## Implementation under verification

- Implementation commit: `d011bcf1cb43369245579340020bae9d0c08bd63`
- Verified with: `git rev-parse d011bcf`
- The commit changes only `README.md`, `INSTALL.md`, and `docs/architecture.md`; those files were not edited during this rework.
- Canonical requirement: `go.mod` declares `go 1.26.9`.

## Verification record

All commands were run from the repository root after aligning the two additional current guidance files.

### Documentation claims gate

Command:

```sh
make docs-check
```

Observed:

```text
go test -short -count=1 -run 'TestDocsClaims' ./cmd/server
ok  	github.com/crier-dev/crier/cmd/server	12.871s
```

Result: PASS.

### Stale-floor grep over the current guidance set

Command:

```sh
if grep -nE 'Go 1\.26\.6\+|Go 1\.26\.6 or later|go\.mod requires go 1\.26\.6|Language:\*\* Go 1\.26\.6\+' README.md INSTALL.md docs/architecture.md CONTRIBUTING.md examples/ws-mesh-demo/README.md; then
  echo 'FAIL: stale Go 1.26.6 floor remains in current guidance set'
  exit 1
else
  echo 'PASS: no stale Go 1.26.6 floor in current guidance set'
fi
```

Observed: no matches; `PASS: no stale Go 1.26.6 floor in current guidance set`.

### Current-document scan

Dated dogfood records and `docs/performance.md` are historical observations, not current setup guidance. This report is excluded because it necessarily records the stale string being checked.

Command:

```sh
if git grep -n '1\.26\.6' -- '*.md' ':!docs/dogfood/**' ':!docs/performance.md' ':!reports/README-004-verification.md'; then
  echo 'FAIL: current Markdown still contains a Go 1.26.6 claim'
  exit 1
else
  echo 'PASS: no current Markdown contains a Go 1.26.6 claim'
fi
```

Observed: no matches; `PASS: no current Markdown contains a Go 1.26.6 claim`.

### Scope and historical fixture

`git diff --exit-code d011bcf1cb43369245579340020bae9d0c08bd63 -- README.md INSTALL.md docs/architecture.md` produced no diff, confirming the implementation commit's three documentation files remain unchanged.

`Makefile:201` is intentionally historical fixture commentary. It quotes the simulated `go1.26.6: not downloaded` failure used by QA-CRIER-40 to explain the regression test; it is not current installation guidance and remains unchanged.

`git diff --check` completed with no output and exit status 0 (PASS).

## Traceability closeout

ch:trace row=README-004 spec=none:no-spec-owns-this wave=crier-foreman-2026-10-19-23-27-03#README-004 test=Makefile::docs-check doc=README.md,INSTALL.md,docs/architecture.md,CONTRIBUTING.md,examples/ws-mesh-demo/README.md prompt=/tmp/crier-foreman-2026-10-19-23-27-03-README-004-rework-brief.txt evidence=reports/README-004-verification.md witness=path@origin:go.mod verdict=none:not-judged commit=d011bcf1cb43369245579340020bae9d0c08bd63 memory=none:worker-no-memory-write

STATUS: VERIFIED
TIER: Tier 1 documentation gate PASS
EVIDENCE: `reports/README-004-verification.md`; `make docs-check`, stale-floor grep, current-document scan, and `git diff --check` all PASS
