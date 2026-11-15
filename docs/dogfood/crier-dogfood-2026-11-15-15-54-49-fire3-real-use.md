# crier dogfood — real-use run, fire 3 (tick `crier-dogfood-2026-11-15-15-54-49`)

- Date: 2026-11-15 (fire 3 of this tick id; fire 1 = `2026-11-15a`, fire 2 = `2026-11-15b`, both delta/consumer-inbox runs)
- HEAD measured: `c859600a84bf965d0c84ed3c294a28fcb3030967` (`== origin/main`, `git rev-list --left-right --count HEAD...origin/main` = `0 0`, clean worktree)
- Binary: `make build` → `crier v0.6.1-230-gc859600a`
- Lane: `crier-dogfood` (skill `coding-hermes-dogfood`; driver `dogfood-scheduler-tick.sh` NOT used — retired)
- Verdict: **SHIPPABLE for the agents/consumer surface, with one P3 contract-truth defect filed.** Nothing a user would feel (perf below). Install leg ran for real on a fresh ephemeral machine: installable, upgradable, 18/24 cells OK.

## What was actually used (the promise a real user acts on)

The promise under test is the harness consumer story: *register agents with a consumer posture, deliver work to them, and be able to audit whether each consumer is wired correctly.* A user does it over the documented HTTP contract, so this fire used the HTTP contract only — no internal packages, no test fixtures.

1. Started a server on `:18791` (`CR_REQUIRE_AGENT_SIG=false` — the documented local mode) and a first-party webhook sink on `:18899`.
2. Registered four agents over the documented `POST /agents`:
   - `wired-agent` — consumer `reader=agent`, `cadence=60`, `ttl=3600`, async webhook `/echo`
   - `bare-agent` — no consumer, no webhook (the "did I forget to wire it?" case)
   - `blocking-no-token` — `reader=harness`, `cadence=30`, blocking webhook `/noecho`
   - `blocking-good` — blocking webhook `/echo` with `require_response_token: true`
3. Delivered work via `POST /agents/<id>/inbox` and watched what the sink received and what the API answered.
4. Drove the three response-token modes: `/echo` (correct token echoed), `/wrongtoken` (wrong token), `/noecho` (no token header).
5. Cleared and re-set `archived_reason` with `PATCH /agents/{id}`, and re-read the census after each change.
6. Read `GET /harness/consumer-census` after every state change.
7. Fetched the served contract `GET /openapi.json` and compared it to the routes actually mounted.
8. Re-ran the server with `CR_AUTH_TOKEN` set and probed both surfaces without a token.

## Findings

### DF-CRIER-505 (P3, pending) — the census endpoint works but is not in the contract

`/harness/consumer-census` is real and genuinely useful: after archiving, it immediately returned

```json
{"agent_id":"archived-agent","state":"archived","reason":"dogfood probe: retired consumer"}
```

and it is auth-guarded (`CR_AUTH_TOKEN` set → census `401`, `/agents` `401`).

But it does not exist in the contract a consumer reads:

| surface | result at this HEAD |
|---|---|
| served `GET /openapi.json` | OpenAPI `3.1.0`, **70** paths, `paths \| keys \| grep harness` = **0** |
| `cmd/server/openapi.yaml` | `grep -c consumer-census` = **0** |
| `docs/openapi.yaml` | `grep -c consumer-census` = **0** |
| `cmd/server/testdata/protocol-census.json` | profile `a2a=off,dagger=off,fed=off` = 72 routes, harness matches **0** |
| `cmd/server/protocol_census_test.go` | no reference |

And the repo's own gates do not see the gap: `go test ./cmd/server -run 'TestProtocolRouteCensusParity|TestClaims' -count=1` → `ok … 0.292s`, exit 0, on the tree where the route is undeclared. Behaviour **is** covered (`ci.yml` runs a `Harness consumer census contract` step inside `go test ./internal/ci`), which is exactly why the drift survives: behaviour is tested, the published interface is not.

Impact: a client-codegen or spec-diff pass silently omits the one endpoint that tells an integrator whether their consumers are wired correctly. `docs/harness-consumer-posture.md` documents the posture but never names the endpoint.

### QA-CRIER-066-R3 (P3, pending) — `collect` pulled nothing, third fire in a row

`bunker-qa.sh collect --evidence /tmp/bunker-qa-evidence-20261115T181255Z-3667281.jsonl` exited 0 with one line of output and produced a stub/meta, not the ledger — while the agent was demonstrably alive over the harness's own transport:

```
bunker exec cb6c5ba1 --server bunker-las-03 -- bash -lc 'wc -l ~/qa-evidence.jsonl'
→ 24
bunker exec cb6c5ba1 --server bunker-las-03 -- bash -lc 'ls -la ~/qa-logs-512794'
→ 25 entries incl. ci.log, tests.log
```

So the ssh path collect relies on was up and the failure is inside collect's probe/merge. This recurs `QA-CRIER-066-R2` on a *new* agent on the same host with a working exec path, which refutes the "agent/host-specific flake" reading of R2. Evidence was hand-pulled over `bunker exec` before teardown and is committed with this record (see below). The cost is real: the agent home — the only copy — is destroyed at teardown, so every fire re-derives evidence a working `collect` would have committed.

### Not filed, but recorded: the act `ci-pass` red is now attributable to one step

Fire 2 reproduced the `go test ./internal/ci` act chain green (19.5 s cold / 0.85 s warm). This fire's real battery run reached much further and failed on exactly one step:

```
[CI/Build & Test] ⭐ Run Main Client round-trip (crier keygen + Python/TS clients, no openssl)
[CI/Build & Test]   ❌  Failure - Main Client round-trip (… no openssl) [186.100729ms]
```

Everything before it passed, including `Harness consumer census contract`, and the step failed in 186 ms with **zero output lines**. `scripts/client-roundtrip.sh:161` hard-requires `go curl ss python3` on PATH. A 186 ms silent exit is consistent with a precondition abort (`ss` is iproute2 and is not part of the `node:22-bookworm` base), but that is a **hypothesis, not a measurement**: the local probe needed to confirm it was refused by the unattended consent gate, so it is written down as unproven rather than filed as a row. It is recorded here because it changes R2/DF-CRIER-497-R2 from "act is broken" to "one step of one workflow fails silently under act" — and because the ledger cannot show even this much: the committed `ci-pass` detail **truncates at `Harness consumer census contract`**, so the ledger alone would have blamed the wrong step. That truncation is DF-CRIER-497-R2's subject and is why the pulled verbatim `ci.log` exists alongside it.

Also observed and **explicitly not a finding**: `bunker-mvp` is unusable for this leg (`PREFLIGHT FAIL: target host bunker-mvp disk usage 96% exceeds 94% launch threshold`, reproduced twice), and `--server bunker-las-03` as a flag is ignored by `bunker-qa.sh` — only `BUNKER_QA_SERVER=bunker-las-03` takes effect. Host capacity is owner/ops territory, not a crier defect.

## Performance (Step 2b) — nothing a user would feel, no PERF row

| measurement | value |
|---|---|
| `GET /harness/consumer-census` warm, single shot, 20 samples | p50 **0.45 ms** |
| blocking webhook, wrong token (rejected) | **502** in 1.198 ms |
| blocking webhook, no `require_response_token` (accepted) | **200** in 1.721 ms |
| cold process start (`crier -version`) | **0.04 s**, 29,864 KB max RSS |

No runtime profile was taken because no number justifies one: every path above is three orders of magnitude below the threshold where a user notices, and `crier` has no SQLite hot path in this journey to `EXPLAIN QUERY PLAN`. Filing a PERF row here would devalue the real ones.

## Install leg — real, on a fresh ephemeral machine

- Host: `bunker-las-03` (only viable host; `bunker-mvp` refuses at 96% disk), selected with `BUNKER_QA_SERVER=bunker-las-03`.
- Agent: `cb6c5ba1` — created, bootstrapped, battery run, evidence pulled, then **destroyed** (`bunker destroy cb6c5ba1 --server bunker-las-03`, exit 0) so nothing is left billing.
- 24-cell ledger: **OK 18 · INFO 3 · N/A 1 · FAIL 1 (`ci-pass`) · UNVERIFIED 1 (`ci-act`)** — the same class as the 2026-11-12 battery.
- Installability proven, not assumed:
  - `fresh-install` **OK** — `go build ./...` fetched every dependency and compiled (no artifact expected for a compile leg).
  - `upgrade` **OK** — previous release `v0.6.0` tree, then reinstall of HEAD, clean.
  - `docker_build`, `compose_config`, `lint`, `tests`, `ui-probe`, `toolchain-bootstrap` ×9 (Go 1.26.10 floor), `chaos-shutdown`, `chaos-errorpath` — all **OK**.
  - `chaos-resource` INFO (OOM under the 3 GiB cap, `internal/webhook` 16.8 s — expected for a heavy suite under the cap, and the doctored env is visible in the row: `child_saw=3145728`).
  - `docker-deploy` INFO, `chaos-disconnect` INFO, `chaos-corruption` N/A — unchanged harness classes.

## What this fire left in the repo

- `docs/dogfood/crier-dogfood-2026-11-15-15-54-49-fire3-real-use.md` — this report.
- `docs/dogfood/crier-dogfood-2026-11-15-15-54-49-qa-evidence-fire3.jsonl` — the 24-row install ledger, pulled verbatim off the agent before teardown.
- `docs/dogfood/crier-dogfood-2026-11-15-15-54-49-qa-ci.log` / `…-qa-tests.log` — the per-cell logs pulled verbatim (112 / 50 lines); `ci.log` is the only artifact that names the failing step.
- `.coding-hermes/evidence/` — byte-identical copies of the three evidence files, next to the other foreman evidence.
- `.coding-hermes/board/tasks.jsonl` — `DF-CRIER-505`, `QA-CRIER-066-R3` (both P3, pending); `.coding-hermes/board/events.jsonl` — one `task_created` event each.
- `.coding-hermes/dogfood-log.md` — entry `2026-11-15c`.

No product source file was modified by this run, and no row was closed: findings go to the foreman as rows.
