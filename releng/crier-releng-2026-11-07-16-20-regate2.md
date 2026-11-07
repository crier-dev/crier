# crier releng — re-gate pass 4, tick crier-releng-2026-11-07-16-20-57

Fourth delivery of the same scheduler tick. Delta-only record; the full census,
CI table and blocker analysis live in `releng/crier-releng-2026-11-07-16-20.md`.
This pass re-probes the blocker at the current tip instead of carrying it.

## Decision

**NO RELEASE CUT — unchanged.** Decision-1 rule 2: the exact-SHA hosted gate
carries an executed red leg → no cut; repair the gate. Next candidate remains
**v0.4.0 (minor)**; nothing was tagged, published, or mutated.

## Delta since the previous pass (3b675e82)

| Item | Previous pass | This pass |
|---|---|---|
| Candidate SHA | `3b675e82` | `644bbedb6b2ed7acdc4e5e17bda6fc1311050d17` |
| Code tree | `3b675e82` | **identical** — `git diff --name-only 3b675e82 644bbedb` = `.coding-hermes/board/events.jsonl`, `.coding-hermes/board/tasks.jsonl`, `releng/crier-releng-2026-11-07-16-20-final.md` (bookkeeping only) |
| Product CI (`ci.yml`) | 42395301630 6/6 success | **42396841275 — terminal SUCCESS, 6/6 jobs** (17:32:46Z push, concluded 17:43Z) |
| `pages` | 42395301624 success | **42396841203 success** |
| `bunker-e2e` | 42395301627 failure | **42396841320 failure**, same job / same step / same root cause |
| Remote parity | HEAD = origin/main | HEAD = origin/main = `git ls-remote origin refs/heads/main` = `644bbedb`; tree clean |

CI run ids on the candidate:
- product https://github.com/crier-dev/crier/actions/runs/42396841275 (6/6 success:
  Whole-board validation, Build & Test 1.26.10, Coverage, Integration Tests,
  OpenAPI Spec Validator, Release acceptance matrix)
- bunker-e2e https://github.com/crier-dev/crier/actions/runs/42396841320
- pages https://github.com/crier-dev/crier/actions/runs/42396841203

## Blocker (unchanged, re-probed at the new tip — not carried)

Run 42396841320 on `644bbedb`, job `Bunker config-matrix E2E`:
`Build & publish image (ghcr)` = success, `Agent-ecosystem battery (compose
stack)` = skipped, `Bunker config-matrix E2E` = **failure** at step `Ensure
bunker agent exists + heartbeat` (17:33:44Z):

```
bunker: server "***" not found in config
##[error]Process completed with exit code 1.
```

The job RUNS on runner `bunker-las-02` and fails its own precondition, so this
is the target-config class (`BUNKER_SERVER` naming a server absent from the
runner's bunker client config), not an absent/unverifiable runner leg. That is
exactly the distinction that forbids retiring it again: the v0.3.0 cut retired
this leg once under INT-CI-019 (no registered runner), INT-CI-019 then
provisioned the runner and closed, and the leg now executes — so it is a real
gate again, and red.

Owning repair row: **INT-CI-030** (pending, P1, root-caused at commit
`3b675e82`). RELEASE-014 stays open and is refreshed at `644bbedb`.

## Release-gate prerequisites still outstanding

- `CHANGELOG.md` `[Unreleased]` = `_Nothing yet._`; no dated `## [0.4.0]`
  section exists. `docs/releases.md` requires the tag point to contain its own
  entry, so a prep commit (+ `README.md` release pin, docs/releases.md §1.4)
  plus a fresh CI cycle is required before any tag. Filed as **RELEASE-015**
  (dispatchable in parallel with INT-CI-030).
- v0.4.0 absent locally and at origin (`git ls-remote origin 'refs/tags/v0.4.0*'`
  → empty; `gh release list` tops out at v0.3.0). No force-move, no re-tag.
- v0.3.0 baseline intact: tag `f38ee7dd` → commit `e950fdb1`, non-draft,
  non-prerelease Release with 8 assets.
- Release driver present and unused this tick: `make release VERSION=…`,
  `make release-artifacts`, `make release-upload`, `.github/workflows/release.yml`.
  Generic `gh release create` fallback was NOT used.

## Census (`v0.3.0..644bbedb`, code-equivalent to the previous pass)

73 commits = 13 merges + 60 non-merge. Non-merge subjects:
board 25, docs 9, test 8, fix 7, chore 5, foreman 3, qa 1, pm 1, feat 1 —
sum 60, reconciled against `git rev-list --count` (73 − 13 = 60).
Zero `BREAKING CHANGE` footers, zero `type!:` subjects. Material product
changes: 1 `feat(harness)` + 7 `fix(...)` (ingress harness binding DF-CRIER-500,
buildinfo version dedupe DF-CRIER-502, releng publisher idempotency RELEASE-012,
load-insensitive test fixes INT-CI-027/INT-CI-029). Semver signal = **minor**.

## Deployment posture (separate finding, corrected)

Live `:8767` (pid 476883) serves `{"version":"0.2.0-465-gd3b78329","commit":
"d3b78329","build_time":"2026-11-06T13:44:14Z","modified":false}`.
`d3b78329` is an ancestor of `origin/main`, **80 commits behind**, and predates
the `v0.3.0` tag (`e950fdb1`, 15:34Z vs 13:44Z build). So: STALE — yes.
DIRTY — **no longer**: `modified:false` refutes the "stale and dirty" half of
RELEASE-011's standing text. Rotation to a released build is still owed.

## Gates not claimed

- No fresh candidate-bound GitReins verdict was produced for `644bbedb`; the
  decision is NO-CUT, so no tag-gate verdict is required and none is claimed.
  Prior verdicts are bound to earlier task commits and are not substituted.
- No dogfood/QA re-run was performed; existing QA PASS evidence is bound to
  2026-11-06 work, not to this candidate, and does not clear the red leg.
