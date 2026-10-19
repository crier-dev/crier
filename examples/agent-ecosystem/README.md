# Agent Ecosystem — crier as the bus between popular agent systems

This directory is a **runnable reference setup**: crier (the agent-to-agent
message bus) wired to several popular agent runtimes — **Pi Agent**, **OpenCode**,
**Claude Code**, **Codex**, **Aider**, **Goose**, **Hermes** — plus a plain
webhook sink and a full battery of tests.

Every piece of the project's design is demonstrated here:

| Piece | Where |
|---|---|
| Agent registry + webhook delivery (blocking / async / batch) | every service self-registers; `battery` exercises all lanes |
| Schema templates (openai-compatible / generic / custom) | agents use `schema_template: generic` (the template's own `raw` extraction — a webhook-level `response_map` is refused with 400) |
| LLM message guard (allow / block / **sanitize-rewrite**) | `guard.policies` on each agent; real DeepSeek verdicts with `DEEPSEEK_API_KEY` |
| Per-channel policies, provider presets | policy `id: default` → deepseek preset; override in compose env |
| Kanban output lane | guard → `hermes kanban` / HTTP writer (see CR-FEAT-014) |
| Federation (CR-FEAT-006) | run the stack on two hosts and point agents at both criers |

It works **anywhere Docker runs** — your laptop, a CI runner, or a bunker agent
(rootless dockerd). On a bunker: scp this directory to the agent, `docker
compose up -d`, done (compose plugin v2.39+ installed on the agent).

## Quick start

```bash
# 1. bring up the stack (crier + sink + pi-agent + opencode + claude-code + codex + aider + goose)
#    The battery is profile-gated: `up` never runs it (DF-CRIER-88/90).
docker compose up -d --build

# 2. run the full battery of tests — ONE-SHOT and explicit (compose profile `battery`).
#    It builds its own image if missing and never rebuilds/recreates the stack, so it can
#    be re-run without resetting crier's in-memory registry.
docker compose --profile battery run --rm battery
#    → 10+ probes: health, round-trips through the bus, async delivery,
#      LLM guard matrix (with DEEPSEEK_API_KEY set)

# 3. with real runtime execution + guard verdicts (set credentials in your shell
#    or .env; do not commit them). CLI harnesses ALSO need their live opt-ins:
export OPENCODE_LIVE=1 CLAUDE_CODE_LIVE=1 CODEX_LIVE=1 AIDER_LIVE=1 GOOSE_LIVE=1
# DEEPSEEK_API_KEY must be exported / present in .env for BOTH commands.
# Each CLI still needs its own valid provider/auth configuration.
docker compose up -d
docker compose --profile battery run --rm battery
```

To rebuild only the battery image after editing `battery.sh` (never add `--build` to the
`run` command — it rebuilds the whole dependency graph and recreates the crier container):
`docker compose --profile battery build battery`.

The stack publishes eight host ports — crier `18767`, sink `19002`,
pi-agent/opencode/claude-code/codex/aider/goose `19101`–`19106` — and each one is an
**override, not a requirement**: set `CRIER_HOST_PORT`, `SINK_HOST_PORT`,
`PI_HOST_PORT`, `OPENCODE_HOST_PORT`, `CLAUDE_CODE_HOST_PORT`, `CODEX_HOST_PORT`,
`AIDER_HOST_PORT`, `GOOSE_HOST_PORT` to move any of them (defaults in the
[Config knobs](#config-knobs) table), e.g.
`CRIER_HOST_PORT=30001 SINK_HOST_PORT=30002 docker compose up -d --build` on a host
where a default is already taken. To run two stacks side by side, also give each one
its own project name — `docker compose -p lab-a up -d --build` (repeat `-p lab-a` on
every compose command for that stack).

Harness PASS means **real runtime execution**, not merely HTTP delivery: the
consumer must finish its CLI (exit 0) or Pi SDK prompt and emit non-empty text,
`"live": true`, a non-empty `exec`, and positive integer `stdout_bytes`.
The battery parses those fields from crier's generic/raw `reply` object; a
reply substring alone cannot pass. CLI errors, auth errors, timeouts and empty
output return HTTP 500 with `error` and **no `reply`**. Pi uses
`exec: "sdk:@earendil-works/pi-coding-agent"`; its `stdout_bytes` counts emitted
SDK text, not an OS stdout stream.

Each harness now receives **two different prompts**, each with a fresh UUID in
`payload.battery_nonce` (the payload crosses the webhook mapping; no forwarded
custom HTTP header is assumed). The consumer asks the runtime to emit
`[crier-battery-nonce:<uuid>]`, extracts `nonce` only from completed runtime
output, and computes `stdout_sha256` over the trimmed UTF-8 reply. The checker
requires both that field and the exact output marker to match this request.
Missing/wrong/replayed nonces fail. The second answer must have a different
`content_sha256` after removing nonce markers, so simply appending a fresh marker
to the same canned text also fails. Both prompts must pass for one harness PASS.
This is request-binding/replay detection under the checked-in consumer, **not
cryptographic runtime attestation**: a malicious consumer that synthesizes fresh
nonce-aware bodies can still lie. It proves neither answer quality nor a
particular upstream provider/model.

With no `DEEPSEEK_API_KEY`, all six harness execution checks explicitly SKIP
(canned mode / no credentials). Health, sink and async transport checks may
still PASS, but **none of those proves a harness ran**. A key with a missing
`*_LIVE=1` opt-in is a misconfigured live run: `live:false` is a **FAIL**, not a
skip. A DeepSeek key does not authenticate every vendor CLI; configure each
runtime's provider credentials separately.

Expected no-key output (not a transcript of live execution):

```
SKIP  round-trip pi-agent via crier (no DEEPSEEK_API_KEY — canned mode; harness execution not tested)
SKIP  round-trip opencode via crier (no DEEPSEEK_API_KEY — canned mode; harness execution not tested)
SKIP  round-trip claude-code via crier (no DEEPSEEK_API_KEY — canned mode; harness execution not tested)
SKIP  round-trip codex via crier (no DEEPSEEK_API_KEY — canned mode; harness execution not tested)
SKIP  round-trip aider via crier (no DEEPSEEK_API_KEY — canned mode; harness execution not tested)
SKIP  round-trip goose via crier (no DEEPSEEK_API_KEY — canned mode; harness execution not tested)
SKIP  guard matrix (no DEEPSEEK_API_KEY — guard runs fail-open)
== battery done: 5 pass / 0 fail / 7 skip (evidence /evidence/ecosystem.jsonl)
```

A fully configured, successful live run has `13 pass / 0 fail / 0 skip`.
An exit code of 0 with SKIPs is **not** full execution coverage.

### Auditable evidence in the checkout

The same `docker compose --profile battery run --rm battery` command keeps the
`battery-evidence:/evidence` named-volume behavior and, on exit (success or
failure), copies that JSONL to a bind mount at `./battery/evidence/ecosystem.jsonl`
relative to this compose file. From the repo root this is:
`examples/agent-ecosystem/battery/evidence/ecosystem.jsonl`.
A failed evidence copy makes the run fail; retain the console output alongside
the JSONL. Do not use `docker compose down -v` if you want to keep the volume.
The file is an accumulating log, not a fresh per-run verdict: use the latest
`["battery","start", ...]` boundary and inspect every harness `result`.
Harness records include `PASS`/`FAIL`/`SKIP`, the reason, HTTP status and the full
response body (including execution markers). Each attempted prompt also records
`run_nonce`, `run_started_at`, `request_started_at`, `prompt_index`, `prompt`,
`nonce`, `nonce_echo`, `nonce_matched`, `stdout_sha256`, `content_sha256` and
`previous_content_sha256`. The run UUID groups records; the request UUID provides
freshness (timestamps are audit context, not a claimed runtime clock check).
A no-key SKIP has no request nonce and `nonce_matched: null`, explicitly untested.
Evidence may contain prompt/output text; review it before committing or sharing it.
`battery/evidence/df-crier-338-offline.jsonl` is explicitly labelled transport
fixture evidence for all six harness ids, NOT vendor-runtime execution evidence.

For a local shell run, `battery/battery.sh` defaults to
`battery/evidence/ecosystem.jsonl`. Override `EVIDENCE` for the primary log and
`EVIDENCE_REPO` for an optional second copy. After edits, rebuild the battery
image to include **both** `battery.sh` and `check-harness.py`:

```bash
# From examples/agent-ecosystem (the long-lived stack must already be up):
docker compose --profile battery build battery
docker compose --profile battery run --rm battery
# Inspect from the repo root:
python3 -m json.tool --json-lines examples/agent-ecosystem/battery/evidence/ecosystem.jsonl
# Offline regression checks (fake CLI/SDK/transport; no real provider calls):
node --test examples/agent-ecosystem/battery/execution.test.mjs
python3 examples/agent-ecosystem/battery/test_binding.py
```

## Services

| Service | What it is | Port |
|---|---|---|
| `crier` | the bus (relay + registry + webhook delivery + guard) | 18767 |
| `sink` | python echo agent — simplest consumer | 19002 |
| `pi-agent` | Pi Agent runtime (`@earendil-works/pi-coding-agent`) consumer; real pi SDK session when a key is set | 19101 |
| `opencode` | OpenCode CLI consumer; `opencode run` on receipt with `OPENCODE_LIVE=1` + key | 19102 |
| `claude-code` | Claude Code CLI consumer (`claude -p`); live with `CLAUDE_CODE_LIVE=1` + key | 19103 |
| `codex` | OpenAI Codex CLI consumer (`codex exec`); live with `CODEX_LIVE=1` + key | 19104 |
| `aider` | Aider (python) consumer (`aider --message`); live with `AIDER_LIVE=1` + key | 19105 |
| `goose` | Goose (block/goose, rust binary) consumer (`goose run`); live with `GOOSE_LIVE=1` + key | 19106 |
| `hermes` | Hermes agent (profile-gated; image `hermes-agent:latest`) | — |
| `battery` | the full battery of tests (profile-gated one-shot: `docker compose --profile battery run --rm battery`) | — |

All consumers (pi-agent through goose) run the **same shared consumer**
(`consumer/consumer.mjs`) — parameterized by `AGENT_ID` / `HARNESS` / `PORT` /
`CRIER_URL` / `DEEPSEEK_API_KEY` and the per-harness live-mode env. Each
Dockerfile installs the harness's real runtime; live mode is opt-in. Without
a key or its opt-in, the consumer still returns canned text for manual transport
demos, explicitly marked `"live": false, "canned": true`. Canned mode is not a
successful harness execution test.

## How the agents wire in

1. **At boot**, each agent self-registers with crier: `POST /agents` with a
   webhook (its own `/hook`, `delivery_mode: blocking`, generic schema) and a
   guard policy.
2. **Sending**: anyone POSTs to `crier/agents/<id>/inbox` — blocking returns
   the agent's reply (`200 {reply}`), async returns `202`, batch buffers. The
   minimal blocking request against the published port:

   ```bash
   curl -s -X POST http://localhost:18767/agents/sink/inbox \
     -H 'Content-Type: application/json' \
     -d '{"payload":{"text":"What vegetable is in plot B?"},"sender":"you","session_id":"my-session","delivery_mode":"blocking","timeout_ms":15000}'
   # → 200 {"reply":"ECHO: What vegetable is in plot B?"}
   ```

   All five fields are required: `payload.text`, `sender`, `session_id`,
   `delivery_mode` (`blocking` | `async` | `batch`) and `timeout_ms` (0–120000).
   The echo agent reads the text out of `payload.text` (or a bare-string
   payload) — any other payload shape still answers `200`, but the reply text
   falls back to the sink's default `ECHO: no text` (crier wraps the sink's own
   body, so the raw response reads `"reply":{"reply":"ECHO: ECHO: no text"}`).
   There is no schema validation on the message payload, so the wrong-shape
   request is a silent trap rather than a 4xx — it is pinned by a battery
   negative-control probe.
3. **Guarding**: every inbound message passes the LLM guard first (DeepSeek
   when a key is present; fail-open otherwise). Injection attempts get a
   structured `403 GUARD_BLOCKED` verdict; salvageable messages get rewritten
   and delivered (`sanitize`).
4. **Receiving**: crier POSTs the message to the agent's webhook per the
   schema template; the agent's reply is extracted and returned to the sender.

## Trying it on a bunker (rootless docker)

The compose file builds `crier` from the repo root (`../..`) — on a bunker you
only ship this directory, so pre-build the images and load them:

```bash
bunker spawn my-lab --server <bunker> --cpu 2.0 --ttl 168h
docker compose --profile battery build              # local build (all images, battery included)
docker save crier-agent-ecosystem-crier:latest crier-agent-ecosystem-sink:latest \
  crier-agent-ecosystem-pi-agent:latest crier-agent-ecosystem-opencode:latest \
  crier-agent-ecosystem-claude-code:latest crier-agent-ecosystem-codex:latest \
  crier-agent-ecosystem-aider:latest crier-agent-ecosystem-goose:latest \
  crier-agent-ecosystem-battery:latest | gzip > /tmp/ecosystem-images.tar.gz
scp -r -i ~/.bunker/keys/my-lab examples/agent-ecosystem bunker-my-lab@<host>:~/
scp -i ~/.bunker/keys/my-lab /tmp/ecosystem-images.tar.gz bunker-my-lab@<host>:~/
bunker exec my-lab -- docker load -i ~/ecosystem-images.tar.gz
bunker exec my-lab -- bash -c 'cd ~/agent-ecosystem && CRIER_HOST_PORT=30001 SINK_HOST_PORT=30002 PI_HOST_PORT=30003 OPENCODE_HOST_PORT=30004 CLAUDE_CODE_HOST_PORT=30005 CODEX_HOST_PORT=30006 AIDER_HOST_PORT=30007 GOOSE_HOST_PORT=30008 docker compose up -d'
bunker exec my-lab -- bash -c 'cd ~/agent-ecosystem && docker compose --profile battery run --rm battery'
# poke the stack from outside: http://<bunker-ip>:30001/health etc.
```

### Live bunker verification (2026-08-24, bunker-server / crier-lab)

The stack is deployed and verified live on `bunker-server` (`crier-lab` agent,
default host ports 18767/19002/19101/19102 — override via `CRIER_HOST_PORT` /
`SINK_HOST_PORT` / `PI_HOST_PORT` / `OPENCODE_HOST_PORT` per the agent's port
range). All four agents
(`sink`, `guarded`, `opencode`, `pi-agent`) register and answer online, and the
full battery passes remotely with the guard matrix enabled (real DeepSeek
verdicts): **8 pass / 0 fail / 0 skip** — clean payload 201, injection
`403 GUARD_BLOCKED` with structured verdict. Raw evidence:
`battery/evidence/remote-bunker-server-2026-08-24.jsonl`. This is historical
transit/guard evidence from before DF-CRIER-329; the old reply-substring battery
did not prove runtime execution. Use the execution-marker battery above for
current harness claims.

## Config knobs

| Env | Default | Meaning |
|---|---|---|
| `DEEPSEEK_API_KEY` | — | enables real guard verdicts + real harness runs (pi SDK / opencode / claude-code / codex / aider / goose) |
| `CR_GUARD_ENABLED` | `true` | master guard switch |
| `CR_GUARD_DEFAULT_POLICY` | — | server-wide default policy JSON (provider presets, fail-closed…) |
| `CRIER_REQUIRE_SIG` | `false` | agent signature enforcement (demo wiring keeps it off) |
| `CRIER_HOST_PORT` / `SINK_HOST_PORT` / `PI_HOST_PORT` / `OPENCODE_HOST_PORT` / `CLAUDE_CODE_HOST_PORT` / `CODEX_HOST_PORT` / `AIDER_HOST_PORT` / `GOOSE_HOST_PORT` | 18767/19002/19101/19102/19103/19104/19105/19106 | host port mapping |
| `OPENCODE_LIVE` / `CLAUDE_CODE_LIVE` / `CODEX_LIVE` / `AIDER_LIVE` / `GOOSE_LIVE` | empty | `1` = the harness consumer runs the real CLI on receipt (requires `DEEPSEEK_API_KEY` too); anything else = canned reply |

## Battery coverage

`battery/battery.sh` (JSONL evidence → `battery-evidence` volume / `$EVIDENCE`,
mirrored to `battery/evidence/ecosystem.jsonl` by compose on exit):
health · sink/pi/opencode/claude-code/codex/aider/goose readiness · blocking
request-bound runtime output (two prompts per harness; nonce + digest + live/exec/byte assertions) ·
async fire-and-forget delivery (sink count delta) · guard matrix (clean allow
201, injection 403 GUARD_BLOCKED) when a key is set. Exit code 0 iff zero
failures and the evidence copy succeeded — SKIPs remain incomplete coverage.
