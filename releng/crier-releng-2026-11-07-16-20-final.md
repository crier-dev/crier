Crier release readiness — scheduler tick crier-releng-2026-11-07-16-20-57, final re-gate

Decision: NO RELEASE CUT. Do not create or push v0.4.0. Decision-1 rule 2 applies: exact candidate CI has an executed red bunker-e2e leg. The candidate also has no material changelog entry yet, so the repository release driver cannot be run until a prep commit lands and receives a fresh gate.

Target and identity

- Canonical repository: crier-dev/crier
- Canonical checkout: /home/kara/crier
- Remote used for CI identity: origin git@github.com:crier-dev/crier.git
- Mirror: gitlab git@gitlab.readydedis.com:totalwindup/crier.git
- Default branch: main
- Candidate: 3b675e82ec0c3ea155d7afbe7e22d64e39f4ddc5
- Parity observed: local HEAD, origin/main, and git ls-remote origin refs/heads/main all equal 3b675e82ec0c3ea155d7afbe7e22d64e39f4ddc5
- Working tree: clean
- Baseline: v0.3.0 (newest semantic tag; v0.4.0 absent locally and remotely)

Range and semver

v0.3.0..HEAD contains 72 commits including merges and 59 non-merge commits. The conventional subject census of commit subjects is 1 feat, 7 fix, 0 perf, 0 breaking, and 64 other/bookkeeping or untyped subjects. The range has shipped-source changes, so it is not a zero-material range; the semver signal remains minor, candidate v0.4.0. No BREAKING CHANGE footer or type! subject was found. The range also contains substantial board/docs/test/harness bookkeeping; release notes must be curated from the material changes rather than copied from every commit.

Exact candidate hosted CI

- Product CI run 42395301630: completed SUCCESS at the exact candidate, all 6 jobs successful: Whole-board validation, Build & Test, Coverage, Integration Tests, OpenAPI Spec Validator, and Release acceptance matrix.
  URL: https://github.com/crier-dev/crier/actions/runs/42395301630
- bunker-e2e run 42395301627: completed FAILURE at the exact candidate.
  URL: https://github.com/crier-dev/crier/actions/runs/42395301627
  Failed job: Bunker config-matrix E2E, step Ensure bunker agent exists + heartbeat. Raw log says `bunker: server "***" not found in config`; deploy and isolation steps were skipped. The job executed on the self-hosted runner, so this is a real infrastructure/configuration red, not an absent-runner or vacuous leg.
- pages run 42395301624: completed SUCCESS at the exact candidate.
  URL: https://github.com/crier-dev/crier/actions/runs/42395301624

The exact candidate therefore fails the release invariant. No owner override was supplied.

Repository release tooling

The repository-owned driver is authoritative: docs/releases.md, Makefile targets release / release-artifacts / release-upload / release-upload-selftest, scripts/release-artifacts.sh, scripts/release-upload.sh, and .github/workflows/release.yml. The driver requires an explicit version, clean main, green local gates, a changelog entry already committed, and builds crier + crier-mcp for linux/amd64, linux/arm64, and darwin/arm64 plus install.sh and SHA256SUMS. No fallback gh release create was used.

Cut prerequisites not yet satisfied

- CHANGELOG.md has an empty [Unreleased] section and no dated [0.4.0] section.
- README.md and changelog promotion must be committed together per docs/releases.md before make release can run.
- No v0.4.0 tag or Release object exists.

GitReins and dogfood/QA

- Available GitReins history contains verdict artifacts bound to prior task commits (latest examples include DF-CRIER-500/501, DF-CRIER-502, INT-CI-029, and harness tasks). No fresh candidate-bound full release verdict was produced for 3b675e82; prior degraded board-only guards are not substituted for a current full-tree verdict.
- QA evidence includes PASS live Hermes and opencode delivery artifacts dated 2026-11-06. The older chaos-disconnect evidence remains unverified/failing by design. No QA result clears the exact-head bunker-e2e red.

Deployment posture

The canonical board carries RELEASE-011 as the separate finding for the live :8767 deployment being stale behind v0.3.0. This does not block a source release by itself, but it blocks any claim that the deployed binary is aligned with the newest release.

Previous release

v0.3.0 is a non-draft, non-prerelease GitHub Release with 8 assets at https://github.com/crier-dev/crier/releases/tag/v0.3.0. Its published asset/checksum/install evidence remains recorded by the prior releng artifacts.

Actionable blocker

INT-CI-030 owns the bunker-e2e repair: install the runner user's missing bunker client config or correct the BUNKER_SERVER configuration, then obtain a terminal successful Bunker config-matrix E2E run on a main SHA at or after this candidate. After that, land the dated 0.4.0 changelog/README prep commit, re-run exact-SHA gates, and use the repository release driver. No tag, Release object, or artifact was created by this tick.
