# INT-CI-035 — GitHub Actions CI and release-bookkeeping evidence

Status: complete for exact-head CI evidence; release bookkeeping remains open. No product or workflow source change is warranted. The
historical failures are classified; the release asset mismatch remains an open
release-bookkeeping obligation owned by the release process.

## Historical failures on ed6ca4aa

The `ed6ca4aa2c73af17f36ba0553fb8131b3e250102` head produced three failed
workflow runs, all with no executed product test:

- CI run `42672216502`, `CI`, conclusion `failure`; only job `Coverage (Go 1.26.10)` (`129348884030`) was `skipped` with zero steps.
- Pages run `42672216609`, `pages`, conclusion `failure`; it had zero jobs.
- Bunker run `42672216698`, `bunker-e2e`, conclusion `failure`; only job `Bunker config-matrix E2E` (`129348939255`) was `skipped` with zero steps.

Live `gh run view` and `gh api repos/crier-dev/crier/actions/runs?head_sha=ed6ca4aa2c73af17f36ba0553fb8131b3e250102` readbacks match these IDs and shapes. These are GitHub job-creation/orchestration anomalies, not crier product-test failures: the failed workflows did not run their product cells.

## Successful comparison heads

For `c5f4c8b4640a62c1f37fd7ec1aea49c2a294367d`, CI run `42673703859` completed success with all six jobs: Whole-board validation (fail closed), Build & Test (1.26.10), Coverage (Go 1.26.10), OpenAPI Spec Validator, Release acceptance matrix (built artifacts), and Integration Tests (registry / PostgresStore). The companion pages run `42673703464` and bunker-e2e run `42673703702` also completed success.

For `45eece6e785dcbf39cc10a1bfcb4ba809f55575d`, CI run `42674614969` completed success for the six CI jobs. This is not a commit-level green claim: the same tagged commit has a failed Release run.

## Release-bookkeeping failure

Release run `42677374020` on tag `v0.6.0` / head `45eece6e785dcbf39cc10a1bfcb4ba809f55575d` failed in job `Build & publish the release assets` (`129362284264`), step `Publish the Release object with every asset (scripts/release-upload.sh)`.

The live failed log reports that `v0.6.0` already carried assets with different content. The release uploader refused to replace them silently: several attached binaries had unreadable or divergent build identity and `SHA256SUMS` was refused as the manifest for that refused set. This is a deliberate release-safety refusal, not a product test failure. No `CLOBBER=1` republish was performed. The release mismatch remains open and must be resolved by an explicit release re-publish decision, not by this CI evidence row.

## Current exact-head CI

The current canonical refs are `origin/main` and `gitlab/main`, both at
`a656956b61a2b0de9997ecd8dd10ee87220cea30`. Exact-head GitHub CI run
`42690155956` completed successfully with these six successful jobs: Whole-board
validation (fail closed), Build & Test (1.26.10), Coverage (Go 1.26.10),
OpenAPI Spec Validator, Release acceptance matrix (built artifacts), and
Integration Tests (registry / PostgresStore). Companion run `42690155584`
(bunker-e2e) completed successfully for Build & publish image (ghcr) and Bunker
config-matrix E2E; its Agent-ecosystem battery job was intentionally skipped
with zero steps. Run `42690156060` (pages) completed successfully with its
deploy job. Exact-head CI is terminal green for `a656956b`. The
release-bookkeeping refusal described above remains open and is separate from
this exact-head CI result.

## Local verification

The isolated worker and the hosted CI evidence verified the following:

- `make shell-yaml-check` — PASS; actionlint 1.7.12; 78 files checked, 0 rejected.
- `GOMODCACHE=/home/kara/go-mod-cache GOCACHE=/home/kara/go-build-cache go build ./...` — PASS.
- `GOMODCACHE=/home/kara/go-mod-cache GOCACHE=/home/kara/go-build-cache go vet ./...` — PASS.
- `GOMODCACHE=/home/kara/go-mod-cache GOCACHE=/home/kara/go-build-cache go test ./... -count=1` — PASS.

The bare host Go cache paths are broken symlinks into unavailable storage; the first canonical bare-cache attempt failed before Go executed. That is host environment evidence, not a product failure.

The workflow source was also validated: `.github/workflows/bunker-e2e.yml` gates `docker-image` on push events and `bunker-matrix` on `needs: docker-image`; the relevant workflow syntax check passed. No workflow source edit was necessary.

ch:trace row=INT-CI-035 spec=none:ci-investigation wave=.coding-hermes/waves/crier-foreman-2026-11-09-20-07-49.json test=make shell-yaml-check && GOMODCACHE=/home/kara/go-mod-cache GOCACHE=/home/kara/go-build-cache go build ./... && GOMODCACHE=/home/kara/go-mod-cache GOCACHE=/home/kara/go-build-cache go vet ./... && GOMODCACHE=/home/kara/go-mod-cache GOCACHE=/home/kara/go-build-cache go test ./... -count=1 evidence=docs/INT-CI-035-ci-evidence.md witness=run:42690155956 verdict=none:foreman-pending commit=2ac2fa40b651a3e578fc3c447e372cb97d48bfd7 memory=none:final-ci-pending
