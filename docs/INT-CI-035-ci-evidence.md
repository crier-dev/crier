# INT-CI-035 — GitHub Actions CI and release-bookkeeping evidence

Status: partial/open. No product or workflow source change is warranted. Exact-head
CI for the current canonical main is still pending, and the v0.6.0 release run has
a classified asset-mismatch refusal that remains open for release bookkeeping.

This record corrects the first evidence record (`d8bd0e98`): it includes all three
failed workflow runs for `ed6ca4aa`, does not treat orchestration failures as product
test failures, and does not claim exact-head green status while the current CI run is
non-terminal. Board files, wave manifests, `.gitreins/tasks.yaml`, and release or
deployment files were not changed.

## Scope and source

- Repository: `crier-dev/crier`
- Current canonical main at rework time: `b0b5ee89442a4df160d16179a8c3144df0ba6fa7`
- Earlier investigated heads: `ed6ca4aa2c73af17f36ba0553fb8131b3e250102`,
  `c5f4c8b4640a62c1f37fd7ec1aea49c2a294367d`, and
  `45eece6e785dcbf39cc10a1bfcb4ba809f55575d`
- Workflow source examined: `.github/workflows/bunker-e2e.yml`; its relevant
  `docker-image`/`bunker-matrix` dependency and push condition do not justify a
  source edit based on the skipped run below.
- `make shell-yaml-check` passed with actionlint `1.7.12`: 78 files checked,
  0 rejected.

## Three failed runs for `ed6ca4aa`

All three runs below concluded `failure`, but none executed a product test. They are
GitHub orchestration/job-creation anomalies: the created job(s) were skipped with
zero steps, or no jobs were created at all.

### `CI` run `42672216502`

- URL: https://github.com/crier-dev/crier/actions/runs/42672216502
- Event/head: `push` / `main` / `ed6ca4aa2c73af17f36ba0553fb8131b3e250102`
- Run status/conclusion: `completed` / `failure`
- Only job: `Coverage (Go 1.26.10)` — job `129348884030`
- Job status/conclusion: `completed` / `skipped`
- Steps: zero (`steps: []`)

### `pages` run `42672216609`

- URL: https://github.com/crier-dev/crier/actions/runs/42672216609
- Event/head: `push` / `main` / `ed6ca4aa2c73af17f36ba0553fb8131b3e250102`
- Run status/conclusion: `completed` / `failure`
- Jobs: none (`jobs: []`)

### `bunker-e2e` run `42672216698`

- URL: https://github.com/crier-dev/crier/actions/runs/42672216698
- Event/head: `push` / `main` / `ed6ca4aa2c73af17f36ba0553fb8131b3e250102`
- Run status/conclusion: `completed` / `failure`
- Only job: `Bunker config-matrix E2E` — job `129348939255`
- Job status/conclusion: `completed` / `skipped`
- Steps: zero (`steps: []`)

These facts support an orchestration classification only. No Go test, coverage test,
Bunker battery, or other product test ran in any of these three failed runs.

## Successful six-job CI run for `c5f4c8b`

`CI` run `42673703859` completed successfully for all six jobs at head
`c5f4c8b4640a62c1f37fd7ec1aea49c2a294367d`:

| Job | Job ID | Conclusion |
| --- | ---: | --- |
| `Whole-board validation (fail closed)` | `129356252907` | success |
| `Build & Test (1.26.10)` | `129356253175` | success |
| `Coverage (Go 1.26.10)` | `129360754759` | success |
| `OpenAPI Spec Validator` | `129360754836` | success |
| `Release acceptance matrix (built artifacts)` | `129360754915` | success |
| `Integration Tests (registry / PostgresStore)` | `129360755158` | success |

Run URL: https://github.com/crier-dev/crier/actions/runs/42673703859

## Six-job CI run for `45eece6e` and the separate release failure

`CI` run `42674614969` completed successfully for the six CI jobs at head
`45eece6e785dcbf39cc10a1bfcb4ba809f55575d`:

| Job | Job ID | Conclusion |
| --- | ---: | --- |
| `Build & Test (1.26.10)` | `129352531500` | success |
| `Whole-board validation (fail closed)` | `129352531828` | success |
| `Coverage (Go 1.26.10)` | `129356016763` | success |
| `Integration Tests (registry / PostgresStore)` | `129356016856` | success |
| `OpenAPI Spec Validator` | `129356017019` | success |
| `Release acceptance matrix (built artifacts)` | `129356017104` | success |

Run URL: https://github.com/crier-dev/crier/actions/runs/42674614969

This does not make commit `45eece6e` an exact-head green commit-level result. The
same tagged commit has a separate failed `Release` run, so the release-bookkeeping
half of this row must remain explicitly classified.

### `Release` run `42677374020`

- URL: https://github.com/crier-dev/crier/actions/runs/42677374020
- Event/head: `push` / `v0.6.0` / `45eece6e785dcbf39cc10a1bfcb4ba809f55575d`
- Run status/conclusion: `completed` / `failure`
- Failed job: `Build & publish the release assets` — job `129362284264`
- Failed step: `Publish the Release object with every asset (scripts/release-upload.sh)`

`scripts/release-upload.sh` refused differing assets because `v0.6.0` already carries
assets with different content. The log classifies the refusal as follows:

- `crier_linux_amd64`, `crier_linux_arm64`, `crier-mcp_linux_amd64`,
  `crier-mcp_linux_arm64`, `crier_darwin_arm64`, and `crier-mcp_darwin_arm64`
  were attached with different content; the log reports unreadable build identity
  for those binaries except for `crier-mcp_linux_amd64`, where the same build
  identity was present but 7,654,012 byte offsets differed (size delta 0, bound
  4,096 offsets).
- `SHA256SUMS` was also different and was refused as the manifest of the refused
  binary set.
- `install.sh` was identical and skipped.
- No `CLOBBER=1` re-publish was performed.

Classification: deliberate release safety refusal against silently replacing already
published release assets, not a product test failure. It remains a release
bookkeeping mismatch/open obligation; it must not be collapsed into the six-job CI
green result.

## Current canonical main exact-head status

Current canonical main is `b0b5ee89442a4df160d16179a8c3144df0ba6fa7`.

Live GitHub readback at rework time:

- `CI` run `42681838192` — head `b0b5ee89442a4df160d16179a8c3144df0ba6fa7`,
  status `in_progress`, conclusion not terminal. Exact-head CI is therefore
  **pending**, not green.
- The current CI jobs observed were:
  - `Build & Test (1.26.10)` — job `129378130029`, success;
  - `Whole-board validation (fail closed)` — job `129378130106`, success;
  - `Release acceptance matrix (built artifacts)` — job `129382995405`, in progress;
  - `Integration Tests (registry / PostgresStore)` — job `129382995533`, success;
  - `Coverage (Go 1.26.10)` — job `129382995751`, in progress;
  - `OpenAPI Spec Validator` — job `129382995848`, success.
- `pages` run `42681837245` — job `deploy` (`129378120674`), success.
- `bunker-e2e` run `42681837230` — `Build & publish image (ghcr)`
  (`129378119978`), success; `Agent-ecosystem battery (compose stack)`
  (`129378122647`), skipped; `Bunker config-matrix E2E` (`129378691511`),
  success.

The exact-head CI condition is not satisfied until run `42681838192` is terminal
success. This evidence record intentionally does not claim that result.

## Local verification

Commands run in the isolated worktree:

- `make shell-yaml-check` — exit 0; actionlint `1.7.12`; 78 files checked,
  0 rejected.
- `GOMODCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gomodcache GOCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gocache go build ./...` — exit 0.
- The same writable-scratch-cache environment with `go vet ./...` — exit 0.
- The same writable-scratch-cache environment with `go test ./... -count=1` — exit
  0; all packages passed.

The bare canonical Go cache paths are broken symlinks:
`/var/tmp/hermes-go-mod -> /mnt/usb512/build-artifacts/hermes-go-mod` and
`/home/kara/.cache/go-build -> /mnt/usb512/build-artifacts/go-build`. The first
canonical-cache attempt failed before Go code ran because those targets are
unavailable. That is environment evidence, not a product failure. The successful
local build, vet, and test results above used writable scratch caches instead.

## Acceptance disposition

- The three `ed6ca4aa` failures are fully classified as orchestration anomalies with
  no product test execution.
- The six-job CI success for `c5f4c8b` is recorded.
- The six-job CI success for `45eece6e` is recorded separately from the failed
  `v0.6.0` release run.
- The release asset mismatch and safety refusal are explicitly recorded and remain
  open bookkeeping work; no clobber publish was performed.
- Current exact-head CI remains pending at run `42681838192`.

Therefore this row remains partial/open. Do not mark all acceptance complete from
this record.

ch:trace row=INT-CI-035 spec=none:ci-investigation wave=none:serial-rework test=make shell-yaml-check && GOMODCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gomodcache GOCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gocache go build ./... && GOMODCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gomodcache GOCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gocache go vet ./... && GOMODCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gomodcache GOCACHE=/home/kara/.hermes/cache/scratch/crier-int-ci-035-gocache go test ./... -count=1 evidence=docs/INT-CI-035-ci-evidence.md witness=none:exact-head-ci-pending verdict=none:foreman-pending commit=none memory=none:foreman-pending
