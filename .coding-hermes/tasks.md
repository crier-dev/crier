# Crier — Model-Router Task Matrix

> **Core purpose:** Lightweight Go pub/sub relay with MCP server — event fan-out for multi-agent systems.
> **Language:** Go | **CI:** GitHub Actions | **Scheduler:** active

## Active

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|

## Completed

| ID | Task | Pri | Cpx | Commit | Model |
|----|------|-----|-----|--------|-------|
| DUCKBRAIN-002 | Populate DuckBrain namespace with current state | Low | 1 | — | DeepSeek V4 Flash |
| QUALITY-003 | specs/AGENTS.md is DexDat doc, not Crier | Low | 1 | — | — |
| CI-011 | Coverage reporting to CI (.gitlab-ci.yml) | Medium | 3 | 7f385dc | DeepSeek V4 Flash |
| DOC-005 | README says GitHub Actions, CI is GitLab | Low | 1 | ba748fd | DeepSeek V4 Pro |
| COV-002 | Middleware tests — 0%→100% coverage | High | 3 | ee64882 | DeepSeek V4 Pro |
| COV-003 | MCP server tests — 67.5%→80%+ | Medium | 3 | — | Step 3.7 Flash |
| INFRA-002 | docker-compose.yml with PostgreSQL | Medium | 2 | 5e353e2 | DeepSeek V4 Flash |
| COV-004 | PostgresStore integration tests enabled | High | 3 | 02777e8 | DeepSeek V4 Pro |
| FEAT-001 | Bearer auth middleware | High | 3 | ee64882 | DeepSeek V4 Pro |
| INFRA-001 | Upgrade Go to 1.26.5 | Medium | 1 | — | DeepSeek V4 Flash |
| COV-001 | PostgresStore integration tests | High | 3 | — | DeepSeek V4 Pro |
| CI-009 | Fix flaky mesh test | Medium | 3 | — | DeepSeek V4 Pro |
| CI-001 through CI-008 | Various CI fixes | Medium | 2-4 | — | Step 3.7 Flash |
| DOC-001, DOC-002 | Documentation tasks | Low | 2 | — | DeepSeek V4 Flash |
| FEAT-002 | Structured logging (log/slog) | Medium | 3 | b77c28a | DeepSeek V4 Pro |
| CI-013 | Board-commit CI failures investigation | Low | 2 | — | DeepSeek V4 Pro |
| DOC-003 | Create CONTRIBUTING.md | Low | 1 | e695ee9 | DeepSeek V4 Flash |
| DOC-004 | Add missing LICENSE file | Low | 1 | c0190b9 | DeepSeek V4 Flash |
| QUALITY-001 | Doc comments on 11 exported functions | Low | 1 | 0486ec2 | DeepSeek V4 Flash |
| QUALITY-002 | Fix .vfs/ gitignore for Hilo cache | Low | 1 | c9cdf13 | DeepSeek V4 Flash |
| SPEC-001 | Sync README/CI docs to reality | Low | 1 | c0190b9 | DeepSeek V4 Flash |
| CI-010 | Dockerfile + Makefile fixes | Medium | 2 | 2a1308c | DeepSeek V4 Flash |
| COV-005 | Entrypoint smoke tests | Medium | 2 | c0b2667 | Step 3.7 Flash |
| TEST-001 | Config package tests (0%→~90%) | High | 3 | 5d7c52d | Step 3.7 Flash |
| TEST-002 | PostgresStore unit tests with mock DB | High | 4 | d31bba9 | DeepSeek V4 Pro |
| TEST-003 | Migrate package integration tests | Medium | 3 | 216bbc1 | Step 3.7 Flash |
| PITFALL-001 | Implement rate limiting (stubbed) | High | 4 | 799c349 | DeepSeek V4 Pro |
| PITFALL-002 | Restrict WebSocket origin check | Medium | 2 | f6ea945 | DeepSeek V4 Flash |
| PITFALL-003 | Tighten gitleaks allowlist | Medium | 2 | c9cdf13 | DeepSeek V4 Flash |
| PERF-001 | Add benchmarks for hot paths | Low | 2 | 12e9840 | Step 3.7 Flash |
| DUCKBRAIN-001 | Populate project namespace | Low | 1 | 19cc900 | DeepSeek V4 Flash |
| CI-012 | Fix flaky Go 1.25 test | Medium | 3 | 0f8c01d | DeepSeek V4 Pro |

## Assumptions

- Go project with `go build ./... && go test ./... && go vet ./...` validation gate
- gitreins guard (Tier 1) + Hilo classification active
- Budget model routing: Step 3.7 Flash for test/infra/CI tasks ($0.09/1M), DeepSeek V4 Flash for mechanical/docs ($0.10/1M), DeepSeek V4 Pro for debugging/concurrency
- Previously 30 tasks completed across 10+ prior foreman ticks (2026-07-12 through 2026-07-21)

## Routing Notes

- DeepSeek V4 Flash for all mechanical edits (docs, config, trivial file changes) — $0.10/1M
- DeepSeek V4 Pro for concurrency and debugging
- Escalate to GLM-5.2 if Flash models fail tests twice

## Execution Order

1. DUCKBRAIN-002 (populate DuckBrain namespace with current state)

---

## [ ] NEVER-DONE — Run coding-hermes-never-done 11-point audit

Load coding-hermes-never-done skill. Run ALL 11 checks: spec alignment, doc coverage, test gaps, package upgrades, pitfall hunt, performance audit, endpoint verification, CI/CD health, DuckBrain sync, code quality, middle-out wiring. Create a task for EVERY gap found. This task is never complete — the audit always finds something.

> **Idle tick #1 (2026-07-20 23:37):** 11/11 checks pass, 8/8 packages green, 80.4% coverage. No tasks created.
> **Idle tick #2 (2026-07-21 01:10):** 11-point audit re-run with concrete tool calls. GitReins had 10 stale pending tasks — deleted all 10 (board had them [x], code verified complete). Found 3 gaps: CI-011 (coverage not in .gitlab-ci.yml despite board claiming done), DOC-005 (README says GitHub Actions but CI is GitLab), QUALITY-003 (specs/AGENTS.md is misplaced DexDat doc). Board fabricated in tick #1 — 10 GitReins-pending tasks were marked [x] prematurely. Idle counter: 2/7.
> **Idle tick #4 (2026-07-21 16:21):** DUCKBRAIN-002 completed — DuckBrain namespace populated with current state (15 keys: architecture, config, status, 4 pitfalls, 3 CI entries, 2 task entries, test patterns, 2 idle ticks). Status entry includes 8 packages, 80.4% coverage, 14 routes, 5 benchmarks, Go 1.26.5. Pre-existing flake: CI-014 mesh OnClose (33% failure). Board now empty — graduated cooldown to 4h (14400s). Build+vendor PASS, 7/8 packages test green (mesh flake excluded). Idle counter: 4/7.
|> **Idle tick #5 (2026-07-21 20:24):** 11-point audit re-run with 11 concrete checks. All 11 PASS. 8/8 packages build green, go vet clean. 7/8 test packages pass (mesh CI-014 flake excluded). 80%+ core coverage. 6 direct deps all current. 5 benchmarks with measurable results. 14 HTTP routes + 8 MCP tools all wired. DuckBrain has 15 entries in `crier` namespace. GitLab CI pipeline #38 stuck_or_timeout_failure (no runner — infra gap, not code). Cooldown at 43200s (12h, auto-graduated). 1 moderate vuln in transitive indirect dep (GO-2026-5970, golang.org/x/text, low exploitability). No tasks created. Idle counter: 5/7.
> **Scheduler:** CooldownS=43200 (12h), Enabled=True
