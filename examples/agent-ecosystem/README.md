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
docker compose up -d --build

# 2. run the full battery of tests (--build so a patched battery.sh is picked up)
docker compose run --rm --build battery
#    → 10+ probes: health, round-trips through the bus, async delivery,
#      LLM guard matrix (with DEEPSEEK_API_KEY set)

# 3. with the guard live (real verdicts):
DEEPSEEK_API_KEY=sk-... docker compose up -d
docker compose run --rm --build battery
```

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

Expect (key set → real guard verdicts; no key → guard runs fail-open, matrix skipped):

```
PASS  crier health
PASS  round-trip pi-agent via crier
PASS  round-trip opencode via crier
PASS  round-trip claude-code via crier
PASS  round-trip codex via crier
PASS  round-trip aider via crier
PASS  round-trip goose via crier
PASS  round-trip sink echo via crier
PASS  async deliver (202)
PASS  async delivered
PASS  guard clean payload allowed
PASS  guard injection blocked
battery done: 12 pass / 0 fail
```

(no key → the guard matrix is skipped: `battery done: 10 pass / 0 fail / 1 skip`)

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
| `battery` | the full battery of tests (run with `docker compose run`) | — |

All consumers (pi-agent through goose) run the **same shared consumer**
(`consumer/consumer.mjs`) — parameterized by `AGENT_ID` / `HARNESS` / `PORT` /
`CRIER_URL` / `DEEPSEEK_API_KEY` and the per-harness live-mode env. Each
Dockerfile installs the harness's real runtime; live mode is opt-in so the
battery stays deterministic on any host.

## How the agents wire in

1. **At boot**, each agent self-registers with crier: `POST /agents` with a
   webhook (its own `/hook`, `delivery_mode: blocking`, generic schema) and a
   guard policy.
2. **Sending**: anyone POSTs to `crier/agents/<id>/inbox` — blocking returns
   the agent's reply (`200 {reply}`), async returns `202`, batch buffers.
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
docker compose build                                   # local build (all images)
docker save crier-agent-ecosystem-crier:latest crier-agent-ecosystem-sink:latest \
  crier-agent-ecosystem-pi-agent:latest crier-agent-ecosystem-opencode:latest \
  crier-agent-ecosystem-claude-code:latest crier-agent-ecosystem-codex:latest \
  crier-agent-ecosystem-aider:latest crier-agent-ecosystem-goose:latest \
  crier-agent-ecosystem-battery:latest | gzip > /tmp/ecosystem-images.tar.gz
scp -r -i ~/.bunker/keys/my-lab examples/agent-ecosystem bunker-my-lab@<host>:~/
scp -i ~/.bunker/keys/my-lab /tmp/ecosystem-images.tar.gz bunker-my-lab@<host>:~/
bunker exec my-lab -- docker load -i ~/ecosystem-images.tar.gz
bunker exec my-lab -- bash -c 'cd ~/agent-ecosystem && CRIER_HOST_PORT=30001 SINK_HOST_PORT=30002 PI_HOST_PORT=30003 OPENCODE_HOST_PORT=30004 CLAUDE_CODE_HOST_PORT=30005 CODEX_HOST_PORT=30006 AIDER_HOST_PORT=30007 GOOSE_HOST_PORT=30008 docker compose up -d'
bunker exec my-lab -- bash -c 'cd ~/agent-ecosystem && docker compose run --rm battery'
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
`battery/evidence/remote-bunker-server-2026-08-24.jsonl`.

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

`battery/battery.sh` (JSONL evidence → `battery-evidence` volume / `$EVIDENCE`):
health · sink/pi/opencode/claude-code/codex/aider/goose readiness · blocking
round-trips through the bus ·
async fire-and-forget delivery (sink count delta) · guard matrix (clean allow
201, injection 403 GUARD_BLOCKED) when a key is set. Exit code 0 iff zero
failures — CI-friendly.
