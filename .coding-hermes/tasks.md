# Crier — Model-Router Task Matrix

> **Core purpose:** Lightweight Go pub/sub relay with MCP server — event fan-out for multi-agent systems.
> **Language:** Go | **CI:** GitLab | **Scheduler:** active

## Active

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
| DOC-003 | Create CONTRIBUTING.md | Low | 1 | — | +documentation, ++file-editing | **DeepSeek V4 Flash** | Mechanical doc creation, minimal reasoning needed. Cheapest. | GLM-5.2 |
| COV-005 | Entrypoint smoke tests (cmd/server + cmd/crier-mcp) | Medium | 2 | — | ++testing, +code-generation | **Step 3.7 Flash** | Simple Go test authoring, infra/CI pattern. $0.09/1M budget. | DeepSeek V4 Flash |
| SPEC-001 | Sync README/CI docs to reality | Low | 1 | — | ++documentation, +file-editing | **DeepSeek V4 Flash** | Doc updates, no code complexity. Cheapest model. | GLM-5.2 |
| DOC-004 | Add missing LICENSE file | Low | 1 | — | +documentation, ++file-editing | **DeepSeek V4 Flash** | Trivial file creation. | GLM-5.2 |
| TEST-001 | Config package tests (0%→~90%) | High | 3 | — | +++testing, ++code-generation, +terminal | **Step 3.7 Flash** | Go test authoring, moderate complexity. Budget-friendly. | DeepSeek V4 Pro |
| TEST-002 | PostgresStore unit tests with mock DB | High | 4 | — | +++testing, ++code-generation, +database | **DeepSeek V4 Pro** | DB mocking, multi-condition testing. Needs debugging ability. | GLM-5.2 |
| TEST-003 | Migrate package integration tests | Medium | 3 | — | +++testing, +database, +code-generation | **Step 3.7 Flash** | Integration test patterns, testcontainers. Budget-friendly. | DeepSeek V4 Pro |
| PITFALL-001 | Implement rate limiting (stubbed) | High | 4 | — | ++code-generation, ++concurrency, +testing | **DeepSeek V4 Pro** | Concurrency + sliding-window algorithm. Needs debugging. | GLM-5.2 |
| PITFALL-002 | Restrict WebSocket origin check | Medium | 2 | — | ++code-generation, +security, +testing | **DeepSeek V4 Flash** | Simple config + validation. Low complexity. | Step 3.7 Flash |
| PERF-001 | Add benchmarks for hot paths | Low | 2 | — | ++testing, +performance, +code-generation | **Step 3.7 Flash** | Mechanical benchmark boilerplate. Budget-friendly. | DeepSeek V4 Flash |
| CI-010 | Dockerfile + Makefile fixes | Medium | 2 | — | ++infra, +documentation, +file-editing | **DeepSeek V4 Flash** | Simple Docker/Makefile authoring. | Step 3.7 Flash |
| CI-011 | Coverage reporting to CI | Medium | 3 | — | ++infra, +testing, +code-generation | **Step 3.7 Flash** | CI config + coverage tooling. Infra/CI speciality. | DeepSeek V4 Pro |
| QUALITY-001 | Doc comments on 11 exported functions | Low | 1 | — | +documentation, ++file-editing | **DeepSeek V4 Flash** | Mechanical comment addition. Cheapest model. | Hy3 |
| DUCKBRAIN-001 | Populate project namespace | Low | 1 | — | +documentation, +api-use | **DeepSeek V4 Flash** | Simple data entry. | GLM-5.2 |
| CI-012 | Fix flaky Go 1.25 test | Medium | 3 | — | ++debugging, +testing, +code-generation | **DeepSeek V4 Pro** | Debugging flaky test needs investigation skill. | GLM-5.2 |
| PITFALL-003 | Tighten gitleaks allowlist | Medium | 2 | — | +security, ++file-editing | **DeepSeek V4 Flash** | Config-only change, no code. | Step 3.7 Flash |
| QUALITY-002 | Fix .vfs/ gitignore for Hilo cache | Low | 1 | — | +infra, ++file-editing | **DeepSeek V4 Flash** | Trivial gitignore edit. | Hy3 |

## Completed

| ID | Task | Pri | Cpx | Commit | Model |
|----|------|-----|-----|--------|-------|
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

## Assumptions

- Go project with `go build ./... && go test ./... && go vet ./...` validation gate
- gitreins guard (Tier 1) + Hilo classification active
- Budget model routing: Step 3.7 Flash for test/infra/CI tasks ($0.09/1M), DeepSeek V4 Flash for mechanical/docs ($0.10/1M), DeepSeek V4 Pro for debugging/concurrency

## Routing Notes

- Step 3.7 Flash preferred for Go test authoring and CI tasks — budget-optimized at $0.09/1M
- DeepSeek V4 Flash for all mechanical edits (docs, config, trivial file changes) — $0.10/1M
- DeepSeek V4 Pro for concurrency (PITFALL-001 rate limiter) and debugging (CI-012 flaky test)
- Escalate to GLM-5.2 if Flash models fail tests twice

## Execution Order

1. DOC-003, DOC-004, QUALITY-001, QUALITY-002 (docs mech — parallel)
2. CI-010, CI-011 (infra — parallel)
3. SPEC-001 (doc sync)
4. PITFALL-002, PITFALL-003 (security config)
5. COV-005, TEST-001 (test scaffolding)
6. TEST-002, TEST-003 (DB tests — serial, shared deps)
7. PITFALL-001 (rate limiter — most complex)
8. PERF-001, DUCKBRAIN-001 (low pri)
9. CI-012 (flaky test fix)

## Escalation Conditions

- Concurrency tests fail twice → escalate to GPT-5.6 Sol (complex reasoning)
- DB mock tests fail with unexpected errors → escalate to GLM-5.2
- Rate limiter design iteration >3 → escalate to GPT-5.6 Sol (architecture)

---

## [ ] NEVER-DONE — Run coding-hermes-never-done 11-point audit

Load coding-hermes-never-done skill. Run ALL 11 checks: spec alignment, doc coverage, test gaps, package upgrades, pitfall hunt, performance audit, endpoint verification, CI/CD health, DuckBrain sync, code quality, middle-out wiring. Create a task for EVERY gap found. This task is never complete — the audit always finds something.

> **Last idle tick:** #1 (2026-07-20 23:37) — 11/11 checks pass, 8/8 packages green, 80.4% coverage.
> **Scheduler:** CooldownS=1800, Enabled=True
