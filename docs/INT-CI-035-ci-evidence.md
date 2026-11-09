# INT-CI-035 — GitHub Actions CI investigation

Status: partial — no product or workflow source change is warranted.

The investigated failure is an orchestration/event-classification anomaly, not an
executed crier product test failure. The deliverable for this row is this frozen,
repository-local evidence record. Board files, wave manifests, `.gitreins/tasks.yaml`,
and release/deployment files were not changed.

## Scope and source

- Repository: `crier-dev/crier`
- Canonical evaluated head: `45eece6e785dcbf39cc10a1bfcb4ba809f55575d`
- Workflow source: `.github/workflows/bunker-e2e.yml`
- Relevant source lines at the canonical head: `docker-image` is conditional on
  `github.event_name == 'push'` (line 30); `bunker-matrix` declares
  `needs: docker-image` (line 45); its condition is:
  `always() && (github.event_name == 'workflow_dispatch' || github.event_name == 'schedule' || (github.event_name == 'push' && needs.docker-image.result == 'success'))`
  (line 46).
- Live source validation: `gh api repos/crier-dev/crier/contents/.github/workflows/bunker-e2e.yml?ref=ed6ca4aa`
  matched the checked-out source for the relevant lines.
- Workflow validation: `make shell-yaml-check` passed with actionlint 1.7.12;
  78 files checked, 0 rejected.

## Failed bunker run

Run `42672216698` (`bunker-e2e`):

- URL: https://github.com/crier-dev/crier/actions/runs/42672216698
- Event: `push`; branch: `main`
- Head: `ed6ca4aa2c73af17f36ba0553fb8131b3e250102`
- Run conclusion: `failure`; status: `completed`
- Only job: `Bunker config-matrix E2E`
- Job conclusion/status: `skipped` / `completed`
- Job URL: https://github.com/crier-dev/crier/actions/runs/42672216698/job/129348939255
- Steps: zero (`steps: []`)

The live run has no `docker-image` job and the only job was skipped with no
steps. Under the checked-in condition, the matrix job is eligible for a push only
when `needs.docker-image.result == 'success'`. Therefore this run did not execute
the bunker battery or any product test. The available evidence supports
classification as a workflow run/event orchestration anomaly; it does not identify
a repository defect or justify changing the workflow condition.

## c5f4c8b run

Run `42673703859` (`CI`) was initially recorded as in progress at the tick start.
The live terminal readback now shows it completed successfully:

- URL: https://github.com/crier-dev/crier/actions/runs/42673703859
- Event: `push`; branch: `main`
- Head: `c5f4c8b4640a62c1f37fd7ec1aea49c2a294367d`
- Run conclusion/status: `success` / `completed`
- Jobs: `Whole-board validation (fail closed)`, `Build & Test (1.26.10)`,
  `Coverage (Go 1.26.10)`, `OpenAPI Spec Validator`,
  `Release acceptance matrix (built artifacts)`, and
  `Integration Tests (registry / PostgresStore)` — all `success`.

## Exact-head green CI evidence

Run `42674614969` (`CI`) is the exact canonical-head verification:

- URL: https://github.com/crier-dev/crier/actions/runs/42674614969
- Event: `push`; branch: `main`
- Head: `45eece6e785dcbf39cc10a1bfcb4ba809f55575d`
- Run conclusion/status: `success` / `completed`
- Jobs: `Build & Test (1.26.10)`, `Whole-board validation (fail closed)`,
  `Coverage (Go 1.26.10)`, `Integration Tests (registry / PostgresStore)`,
  `OpenAPI Spec Validator`, and `Release acceptance matrix (built artifacts)` —
  all `success`.
- The `Build & Test` job's `Shell + workflow YAML check`, `Build`, `Vet`, and
  `Test` steps all completed `success`.

This exact-head green run is the required hosted CI evidence for the current
canonical tree. It also independently verifies the source/workflow validation
surface that could have been affected by a workflow edit; no edit is needed.

## Local verification

Commands run from the canonical worktree:

- `make shell-yaml-check` — exit 0; actionlint 1.7.12; 78 files checked, 0 rejected.
- `go build ./...` — exit 0.
- `go vet ./...` — exit 0.
- `go test ./... -count=1` — exit 0; all packages passed.

The local Go checks validate the current product tree; the hosted exact-head run
is the CI/workflow evidence. No live bunker runner or product battery was
available from the failed run, so no bunker product verdict is claimed for
`42672216698`.

ch:trace row=INT-CI-035 spec=none:ci-investigation wave=.coding-hermes/waves/crier-foreman-2026-11-09-18-46-25.json test=make shell-yaml-check && go build ./... && go vet ./... && go test ./... -count=1 evidence=docs/INT-CI-035-ci-evidence.md witness=run:42674614969 verdict=none:foreman-pending commit=none memory=none:foreman-pending
