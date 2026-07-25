<!--
  ⚠️  BOARD FORMAT — coding-hermes-model-router v1.3 (2026-07-24)
  All tasks MUST use matrix format: | ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
  Before editing this file, load the skill: skill_view(name='coding-hermes-model-router')
  Validate: python3 ~/.hermes/scripts/validate-board-format.py .coding-hermes/tasks.md
- [ ] **GITREINS-JUDGE — Configure LLM evaluator for commit quality review**
  | 🔴 Critical | — | — | deepseek-v4-flash @ deepseek-foreman | GITREINS_LLM_API_KEY in ~/.hermes/.env | foreman-direct |

  Run: `python3 ~/.hermes/scripts/check-gitreins-judge.py .` to verify.
  Default limits (adjust per-project based on codebase size and task complexity):
  - Fast/small projects: `max_iterations: 50`, `max_time: 10m`, tokens: `0.2M/0.4M`
  - Large repos (Go monorepos, 100+ files): `max_iterations: 100`, `max_time: 30m`, tokens: `1M/2M`
  - C++/Rust (slow compiles): `max_time: 30m` minimum
  - Scheduler/production infra: `max_time: 30m`, tokens: `1M/2M`
  Supervisor auto-flags projects where limits are too low for codebase size.

| 🔴 Critical | — | — | deepseek-v4-flash @ deepseek-foreman | GITREINS_LLM_API_KEY in ~/.hermes/.env | foreman-direct |

  Run: `python3 ~/.hermes/scripts/check-gitreins-judge.py .` to verify.
  If missing, create/edit .gitreins/config.yaml with evaluator section using deepseek-v4-flash.
  This is CRITICAL for code quality — no automated review of worker output without it.

  NEVER remove the matrix header row or NEVER-DONE / E2E-001 fixtures.
-->

<!-- ⚠️  CRITICAL: 24 consecutive idle ticks (15 past self-disable threshold).
     Project is complete — 8/8 packages passing, 78.6% coverage, zero actionable gaps.
     This foreman cron should be PAUSED. Run: hermes cronjob remove <job_id>
     Marker: .coding-hermes/CRON_PAUSE_REQUESTED -->

# Crier — Model Router Task Matrix

> **Core purpose:** Lightweight Go pub/sub relay with MCP server — event fan-out for multi-agent systems.
> **Language:** Go 1.26.5 | **CI:** GitHub Actions | **Status:** Zombie — 24 idle ticks. Project complete. CRON_PAUSE_REQUESTED written.

## Active Tasks

| ID | Task | Pri | Cpx | Deps | Tags | Model | Lvl | Fallback |
|----|------|-----|-----|------|------|-------|-----|----------|
| E2E-001 | E2E Testing Tick (self-improving loop) 🔁 Recurring every 5-10 ticks | High | 4 | server running | ++browser, ++screenshots, ++verification | GPT-5.6 Luna | High | Step 3.7 Flash |
| NEVER-DONE | 11-point audit sweep | Medium | 2 | — | ++code-review, +testing | DeepSeek V4 Pro | Medium | GLM-5.2 |

## Completed

30 tasks completed across 10+ foreman ticks. 8/8 packages pass, 78.6% coverage, 14 HTTP routes, 8 MCP tools.

| Phase | Key outcomes |
|-------|--------------|
| Test Coverage | COV-001 through COV-005 — PostgresStore, middleware, MCP server, entrypoint smoke, config package |
| CI Fixes | CI-001 through CI-013 — flaky mesh test, coverage reporting, CI runner issues |
| Docs | DOC-001 through DOC-005 — README sync, CONTRIBUTING, LICENSE, AGENTS.md |
| Quality | QUALITY-001 through QUALITY-003 — doc comments, gitignore, spec alignment |
| Features | FEAT-001 (Bearer auth), FEAT-002 (structured logging) |
| Security | PITFALL-001 through PITFALL-003 — rate limiting, WebSocket origin, gitleaks |
| Performance | PERF-001 — benchmarks for hot paths |
| DuckBrain | DUCKBRAIN-001/002 — namespace with 15 entries |

## Assumptions

- Go project with `go build ./... && go test ./... && go vet ./...` validation gate
- 8/8 packages pass (mesh CI-014 flake pre-existing, resolved in recent ticks)
- 6 direct deps all current (golang-migrate, gorilla/mux, gorilla/websocket, pgx, testify, testcontainers)
- 1 moderate vuln (GO-2026-5970, golang.org/x/text v0.38.0, transitive, advisory)
- GitHub Actions CI (not GitLab — prior board fabricated GitLab CI)
- 23 idle ticks, 15 past self-disable threshold. 17+ cooldown reversions. Fleet TOML root cause.

## Routing Notes

- **NEVER-DONE audit:** Foreman-direct (V4 Pro) — full context, terminal, file search, memory access
- **If new tasks emerge:** DeepSeek V4 Flash for mechanical ($0.10/1M), V4 Pro for debugging/concurrency, GLM-5.2 for Go implementation
- **E2E testing:** GPT-5.6 Luna for browser ($100/mo flat), Step 3.7 Flash for CLI/API ($0.09/1M)
- Project is a zombie — 23 idle ticks, zero actionable gaps, 8/8 green

## Execution Order

1. NEVER-DONE (runs every tick)
2. E2E-001 (periodic)

## Escalation Conditions

- Audit finds new gap → create task, route per capability profile
- Security vulnerability in called code → CRITICAL, escalate to GPT-5.6 Sol
- CI failure (GitHub Actions) → investigate with V4 Pro
- Zombie threshold exceeded → escalate to Bane for project disable
