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
# Export the provider keys in the matrix below; DEEPSEEK_API_KEY also enables the guard.
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

Harness PASS requires **consumer-reported runtime-channel consistency**, not merely
HTTP delivery. The shipped consumer finishes its CLI (exit 0) or Pi SDK prompt
and reports non-empty text,
`"live": true`, a non-empty `exec`, and positive integer `stdout_bytes`.
The battery parses those fields from crier's generic/raw `reply` object; a
reply substring alone cannot pass. CLI errors, auth errors, timeouts and empty
output return HTTP 200 with `status:"FAILED"`, `live:false`, `error` and **no `reply`**
(the HTTP success preserves the diagnostic through crier; it is not harness success). Pi uses
`exec: "sdk:@earendil-works/pi-coding-agent"`; its `stdout_bytes` counts emitted
SDK text written to stdout by its bounded child process.

A harness is **supported** only when both distinct prompts pass. The verifier
mints a fresh single-use challenge and a separate random request UUID. It records
`payload: {battery_id: <request UUID>, text: <prompt>}` and uses only that request
UUID in `session_id`; **the challenge is absent from the entire delivery body**.
Before delivery it opens the separate consumer channel
`GET /battery-output/<challenge>?stream=stdout&battery_id=<request UUID>`.
The consumer correlates the request by `battery_id`, then passes the challenge
only as the per-child environment variable `CR_BATTERY_CHALLENGE`, alongside
`CR_BATTERY_REQUEST_SHA256` (SHA-256 of compact UTF-8 JSON with `battery_id`, then
`text`). Neither value is interpolated into the CLI argv or SDK prompt.

Harness authors must read those environment variables and emit
`[crier-battery-nonce:<CR_BATTERY_CHALLENGE value>]` and
`[crier-battery-request-sha256:<CR_BATTERY_REQUEST_SHA256 value>]` on stdout.
The battery prompt instructs agent runtimes to read their process environment
using a tool; the non-battery one-sentence/no-tools behavior is unchanged.
A template that merely prints its prompt or request body without reading this
out-of-band channel must fail `NOT_EXECUTED`, with a reason naming prompt/request
echo and the missing out-of-band challenge. The checker also rejects a request
that leaks the challenge (including through `session_id`). The consumer never
appends challenge markers to make an unaware runtime pass.

The verifier hashes the capture-file bytes itself (before decoding, trimming or newline conversion),
compares that raw SHA-256 and byte count with `stdout_raw_sha256` and
`stdout_bytes`, then requires strict UTF-8 captured text, trimmed with
ECMAScript `String.trim` semantics, to equal `reply` and its existing trimmed
`stdout_sha256`. Out-of-band nonce/digest echoes and different-content checks
(both challenge markers removed) still apply. Body metadata alone never proves
execution. Missing captured nonce is `NOT_EXECUTED`; observed nonce but missing
body echo is `ECHOED_WITHOUT_RUN`; request/capture/reply mismatches are
`DIGEST_MISMATCH`; an old nonce in either capture or body, or an unminted/reused
challenge, is `NONCE_REPLAY`. `REFUSED`, `TIMEOUT` and `FAILED` remain failures.

The shipped consumer's stdout-only mode tees only the runtime child's stdout;
stderr remains in diagnostics. The existing endpoint without the query still
tees both pipes. The additive `stdout_raw_sha256` hashes original pipe bytes;
`stdout_sha256` still hashes the trimmed reply. Observation starts no runtime
and closes on completion/refusal (35-second idle expiry). `probe-runtime.py`
owns the nonce ledger and capture file; JSONL retains its independently computed
raw digest/byte count and a decoded display copy. `BATTERY_RUNTIME_BASE` overrides
the consumer base URL for direct probes; Compose uses the internal host/port.
Rebuild both consumer and battery images: older in-band templates or consumers
without the raw digest cannot earn supported verdicts.

SUPPORTED means the capture bytes reported by the consumer equal its reply, hashed before decode; it is not attestation against a consumer fabricating both channels.

The fresh single-use out-of-band challenge binds the delivered reply to a separate
consumer-reported capture, not to a verifier-owned OS pipe. A harness that reads
the environment and prints the challenge without doing the requested work is
still indistinguishable. With the trusted shipped consumer's stdout-only pipe
tee, the bytes come from a spawned process; SUPPORTED alone does not establish
that provenance. A malicious consumer can fabricate both channels consistently
and pass without spawning a child. This does not establish model authorship,
answer quality, or provider/model identity.

Named future gap: **verifier-owned child stdout capture**. The verifier/observer
must own the child's stdout pipe and tee its raw bytes into a capture file the
consumer cannot write or substitute; the checker must read that capture, not the
consumer's HTTP observation endpoint. That would establish that the observed
child emitted the bytes even against a fabricating consumer, still not that the
requested work was performed correctly. This capture is not implemented here.
Do not expose this diagnostic stack to untrusted clients. No Crier server change
is required.

| Harness | Runtime command / provider | Required key | Missing-key result |
|---|---|---|---|
| `pi-agent` | `createAgentSession` SDK, `deepseek/deepseek-flash` | `DEEPSEEK_API_KEY` | `REFUSED: missing DEEPSEEK_API_KEY` |
| `opencode` | `opencode run … --model deepseek/deepseek-v4-flash` | `DEEPSEEK_API_KEY` | `REFUSED: missing DEEPSEEK_API_KEY` |
| `claude-code` | `claude -p … --output-format text` | `ANTHROPIC_API_KEY` | `REFUSED: missing ANTHROPIC_API_KEY` |
| `codex` | `codex exec --skip-git-repo-check …` (child `CODEX_API_KEY` alias) | `OPENAI_API_KEY` | `REFUSED: missing OPENAI_API_KEY` |
| `aider` | `aider --model deepseek/deepseek-v4-flash --message …` | `DEEPSEEK_API_KEY` | `REFUSED: missing DEEPSEEK_API_KEY` |
| `goose` | `goose run --no-session --no-profile --quiet --text …` (`GOOSE_PROVIDER=openai`, `GOOSE_MODEL=gpt-4o-mini`) | `OPENAI_API_KEY` | `REFUSED: missing OPENAI_API_KEY` |

Missing/empty keys return immediately as HTTP 200 `{live:false,status:"REFUSED",reason:"missing VARIABLE"}` without starting a runtime; the battery reports a FAIL carrying that reason (only the guard matrix SKIPs without DeepSeek), CLI live opt-ins remain required, and sink/Hermes gain no key requirement.

### Runtime failures and current blockers (DF-CRIER-346)

`REFUSED` means the required provider key is absent/empty: no runtime was started.
`FAILED` means the runtime could not start, failed, produced no stdout, or exceeded
its own budget. `OK` means exit 0 plus nonempty runtime stdout and `live:true`;
the battery still separately requires the nonce/digest checks above.

Every runtime request has `RUNTIME_TIMEOUT_MS=20000` by default (Compose forwards
this knob to all six consumers). Allowed values are integers 1–25000; invalid values
return `FAILED` before spawning. The 25s maximum stays below the battery's 30s
blocking-delivery budget; callers using shorter delivery budgets must lower this knob.
On Linux a timeout kills the isolated runtime process group, including descendants,
with SIGKILL and immediately returns a reason, rather than waiting on inherited pipes.
Pi's SDK import, model selection, prompt and cleanup run inside the same killable
child-process boundary. A timeout is not evidence of a particular provider defect.

A failed response includes `runtime`, resolved `exec` when available, `exit_code`
(null if no exit was observed), `signal`, `timed_out`, `timeout_ms`, and the first
2048 bytes each of `stderr`/`stdout`. `reason` includes the exit code or timeout
and both excerpts, with `(empty)` explicitly shown. The same diagnostic is written
to the consumer log and appears on the battery FAIL line and JSONL `reason`.
Successful stdout is capped at 1 MiB; exceeding it is a diagnosed failure.
Diagnostics can contain provider/user text; review them before sharing evidence.

Current per-harness findings (offline checks, not a claim of vendor success):

- Pi: the installed SDK exposes `ModelRuntime`, and its DeepSeek catalog contains
  `deepseek-flash` and `deepseek-v4-pro`. The consumer explicitly selects
  `deepseek/deepseek-flash` and now preserves SDK `message_end.errorMessage`
  (which may arrive without a thrown exception). An incompatible/missing SDK or
  missing model reports that exact import/model error; the latter says to rebuild
  the Pi image. A stalled import/prompt reports its timeout, never a silent 500.
  A successful paid DeepSeek round-trip has not been verified by this patch.
- Goose: **DeepSeek is not wired for this consumer**. It deliberately selects
  `GOOSE_PROVIDER=openai`, `GOOSE_MODEL=gpt-4o-mini` with `OPENAI_API_KEY`, as in
  DF-CRIER-345; exporting only `DEEPSEEK_API_KEY` cannot run Goose. Missing OpenAI
  credentials still return `REFUSED`. With a present but broken OpenAI credential,
  failures include this provider/model/key mismatch explanation plus the actual
  runtime stderr/exit or timeout. `--no-session --no-profile --quiet` avoids saved
  sessions/extensions and non-answer banners; these flags were checked against
  the installed binary. A DeepSeek-backed Goose answer remains blocked on a
  separately configured provider/key mapping, not on crier delivery.
- OpenCode: still selects `deepseek/deepseek-v4-flash`; startup/provider calls may
  exceed the budget. It now returns the exact stderr/exit or
  `opencode: runtime timeout after …ms` with captured output. No paid OpenCode
  completion is claimed.

With the updated consumer mounted into the existing Pi/Goose/OpenCode images,
network-disabled probes with inert credentials and a 1500ms runtime budget each
returned HTTP 200 / `FAILED` in 1.52s (rounded); all three correctly reported a
runtime timeout with empty stream excerpts. This proves bounded real-image
execution, not upstream authentication or a DeepSeek answer. Stub tests additionally
prove exit 3 / `boom-347`, silent TERM-resistant descendants, empty success, SDK
error events, and known-line success without making paid provider calls.

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
`nonce`, `nonce_echo`, `nonce_matched`, `runtime_nonce_observed`, `outcome`,
`expected_request_sha256`, `request`, `runtime_output`, `capture_ok`,
`stdout_sha256`, `content_sha256` and
`previous_content_sha256`. The run UUID groups records; the request UUID provides
freshness (timestamps are audit context, not a claimed runtime clock check).
Refused harnesses retain their request nonce and `nonce_matched: false`, explicitly unexecuted.
Evidence may contain prompt/output text; review it before committing or sharing it.
`battery/evidence/df-crier-338-offline.jsonl` is explicitly labelled transport
fixture evidence for all six harness ids, NOT vendor-runtime execution evidence.

For a local shell run, `battery/battery.sh` defaults to
`battery/evidence/ecosystem.jsonl`. Override `EVIDENCE` for the primary log and
`EVIDENCE_REPO` for an optional second copy. After edits, rebuild the battery
image to include `battery.sh`, `check-harness.py` and `probe-runtime.py`:

```bash
# From examples/agent-ecosystem (the long-lived stack must already be up):
docker compose --profile battery build battery
docker compose --profile battery run --rm battery
# Inspect from the repo root:
python3 -m json.tool --json-lines examples/agent-ecosystem/battery/evidence/ecosystem.jsonl
# Offline regression checks (fake CLI/SDK/transport; no real provider calls):
node --test examples/agent-ecosystem/battery/execution.test.mjs
python3 examples/agent-ecosystem/battery/test_binding.py
python3 examples/agent-ecosystem/battery/test_capture_binding.py
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
`CRIER_URL` / the harness's own provider key and live-mode env. Each
Dockerfile installs the harness's real runtime; live mode is opt-in. Without
its key the consumer refuses immediately; with a key but no CLI opt-in it returns
canned transport text marked `"live": false, "canned": true`. Neither is a
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
| `DEEPSEEK_API_KEY` | — | enables guard verdicts and pi-agent / opencode / aider runtime credentials |
| `OPENAI_API_KEY` | — | Codex and Goose runtime credentials |
| `ANTHROPIC_API_KEY` | — | Claude Code runtime credential |
| `RUNTIME_TIMEOUT_MS` | `20000` | per-request runtime limit in ms, integer 1–25000; must be shorter than the caller's delivery budget |
| `CR_GUARD_ENABLED` | `true` | master guard switch |
| `CR_GUARD_DEFAULT_POLICY` | — | server-wide default policy JSON (provider presets, fail-closed…) |
| `CRIER_REQUIRE_SIG` | `false` | agent signature enforcement (demo wiring keeps it off) |
| `CRIER_HOST_PORT` / `SINK_HOST_PORT` / `PI_HOST_PORT` / `OPENCODE_HOST_PORT` / `CLAUDE_CODE_HOST_PORT` / `CODEX_HOST_PORT` / `AIDER_HOST_PORT` / `GOOSE_HOST_PORT` | 18767/19002/19101/19102/19103/19104/19105/19106 | host port mapping |
| `OPENCODE_LIVE` / `CLAUDE_CODE_LIVE` / `CODEX_LIVE` / `AIDER_LIVE` / `GOOSE_LIVE` | empty | `1` = the harness consumer runs the real CLI on receipt (requires its own provider key too); anything else = canned reply |

## Battery coverage

`battery/battery.sh` (JSONL evidence → `battery-evidence` volume / `$EVIDENCE`,
mirrored to `battery/evidence/ecosystem.jsonl` by compose on exit):
health · sink/pi/opencode/claude-code/codex/aider/goose readiness · blocking
request-bound runtime output (two prompts per harness; nonce + digest + live/exec/byte assertions) ·
async fire-and-forget delivery (sink count delta) · guard matrix (clean allow
201, injection 403 GUARD_BLOCKED) when a key is set. Exit code 0 iff zero
failures and the evidence copy succeeded — SKIPs remain incomplete coverage.
