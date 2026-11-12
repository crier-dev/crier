# crier real-use run — MCP messaging contract and the restart identity trap (2026-11-12)

Lane: `crier-dogfood-2026-11-12-13-39-15` · workdir `/home/kara/.hermes/stand-in/dogfood/crier`
Target: `crier` (`/home/kara/crier`) at **HEAD 1774dcd7**
Product tree identical to `origin/main` 29834165 (the one commit ahead touches only
`.coding-hermes/board/events.jsonl` and `ops/`), so every number below describes shipped code.

Skill: `coding-hermes-dogfood`, executed directly (no retired driver). No source or product
board change was made by this run; the two rows below are the deliverable.

## The promise being tested

`README.md` sells crier as a message bus you reach over HTTP or MCP: an agent registers,
messages are delivered into a durable inbox, a consumer leases a page, acknowledges it, and the
queue drains. That is the headline operation, so that is what was used end to end.

## What was actually used

Built with `make build`, started the server locally, then drove it through the **MCP stdio
bridge** exactly as the README describes (`CRIER_HTTP_URL`, `CRIER_AGENT_ID`,
`CRIER_MESH_URL`, `CR_REQUIRE_AGENT_SIG=false` for the documented local mode).

Verified end to end (probe `e2e_real.py`, raw output `e2e_real.json`):

| step | tool | result |
|---|---|---|
| register peer | `register_agent` | ok, `public_key: ""`, `status: online` |
| deliver 1 | `deliver_message` | `{"message_id": "...", "agent_id": "..."}` |
| deliver 2 with `idempotency_key=key-abc` | `deliver_message` | ok |
| deliver 2 again, same key | `deliver_message` | ok, **same** `message_id`, `queue_depth` stays 2 |
| read queue | `inbox_stats` | `queue_depth: 2` |
| lease a page | `retrieve_inbox max_messages=10` | 2 messages + `lease_id`, `queue_depth: 2` |
| acknowledge | `ack_messages` | `acked: 2` |
| read queue again | `inbox_stats` | `queue_depth: 0` |

So the core contract is real: delivery is durable across the tool surface, idempotency keys
dedupe (a repeated key does not create a second message), leases and acks actually drain.

Documented details that hold up: message payloads are base64 on the wire and this is stated
(`README.md:933`, `docs/integration-guide.md:383`), so the encoding is not a trap.
Pagination with `limit=100` is honoured: 25/25, 100/175 and 100/395 at depth 395 — a page cap is
not silently swallowing backlog.

The Docker path also works when pointed at the ports the compose file declares
(`CRIER_HOST_PORT` → 8767): `docker compose up -d` reaches `/health` **200 on the first poll**
with postgres `healthy`. The install battery's `docker-deploy` cell reporting `probe 000` is a
probe-target problem in the harness, not a broken container (see diagnostics).

## What breaks for a real user

**1. A restart of a bridge with an ephemeral key leaves a bridge that cannot read, while
startup advertises a fully available tool surface.** (row `DF-CRIER-503`, P3)

Run the bridge twice with the same `CRIER_AGENT_ID` and no `CRIER_AGENT_PRIVATE_KEY_FILE`. The
first run registers the ephemeral key and works. The second run prints, in the same second:

```
INFO  MCP tool surface: all 23 advertised tools are available in this mode mode=remote/bridge tools=23
ERROR bridge identity already exists on the server with a DIFFERENT public key — every signed request from this bridge will be rejected with 401 agent_id=mcp-agent key_source=ephemeral
```

and then the first signed call fails: `remote GET /agents/mcp-agent/inbox?lease=30&max=5: status
401: {"error":"signature verification failed"}`.

The ERROR text is excellent — it names the remedy (point `CRIER_AGENT_PRIVATE_KEY_FILE` at the
registered key, delete the stale agent server-side, or use a fresh server). The problem is its
neighbour: the surface line is the signal a user scans for "is my bridge functional", and it
answers *yes* in the one state where 100% of signed calls 401. Either fail fast, or carry the
identity verdict on that line (`identity=rejected`).

**2. Re-registering an existing KEYLESS agent says "different public key" when both keys are
empty.** (row `DF-CRIER-504`, P3)

`register_agent` for a peer succeeded with `public_key: ""`; registering the *same* id again —
the exact thing a restart does — returns `isError` with `agent already registered with a
different public key` (`internal/registry/store.go:477`). There is no key to reconcile: both
sides are empty. In the documented keyless mode this is a dead end whose advice points at a key
that was never set. Split the case: empty/empty → "agent already exists (keyless); unregister it
or use a new agent id"; empty/different-non-empty → the current accurate mismatch text.

## Performance (Step 2b) — nothing a user would feel, so no PERF row

Measured while using it, not in a benchmark harness:

| operation | n | measured |
|---|---|---|
| bridge spawn → `tools/list` (cold) | 1 | 7.5 ms |
| `deliver_message` | 200 | 52.3 ms total → **0.262 ms/msg**, 200 unique ids |
| `retrieve_inbox` (`limit=100`) | — | 0.33–0.37 ms |
| `ack_messages` | — | 0.34 ms |
| `inbox_stats` | — | 0.2–0.3 ms |
| `docker compose up` → `/health` 200 (warm images) | 1 | first poll (≈3 s after `crier` container start, postgres already healthy) |

Every interactive operation is sub-millisecond and the bulk path sustains ~3,800 msgs/s on one
agent. A user cannot notice any of this, so filing a PERF row would devalue the real findings.

One number did look like a defect and turned out not to be: a single burst retrieve with
`limit=100&lease_seconds=300` at `queue_depth 391` returned 10 messages. Identical requests were
then reproduced three ways — 25/25 at depth 25, 100/175 at depth 175, 100/395 at depth 395 — so
the page cap does **not** bind and the observation is recorded, not filed.

## Install leg — REAL, fresh machine, agent destroyed

`bunker-qa.sh launch/collect` on `bunker-mvp`, agent **be52bcfd**, 24 evidence rows
(`docs/dogfood/crier-dogfood-2026-11-12-qa-evidence.jsonl`):

- OK 19 — `launch`, 9× `toolchain-bootstrap` (go 1.27.4 discovered, gcc/g++ wrappers, make, jq),
  `fresh-install`, `lint`, `tests`, `docker_build`, `compose_config`, `upgrade`, `ui-probe`,
  `chaos-shutdown`, `chaos-errorpath`
- INFO 3 — `docker-deploy` (compose up OK, probe 000 → wrong port probed), `chaos-disconnect`,
  `chaos-resource` (OOM under the 3 GiB cap while compiling `internal/webhook` tests)
- N/A 1 — `chaos-corruption` (no db/state files in repo)
- **FAIL 1 / UNVERIFIED 1 — `ci-pass`, `ci-act`**: `act` starts, caches `actions/setup-go@v6`,
  then fails; the evidence head does not name the failing step, so the CI leg is
  **unverified**, not a crier defect. Same class as the 2026-11-06 run and prior `QA-CRIER-48`.

A fresh machine therefore installs, builds, lints, tests, docker-builds, upgrades and passes the
UI and most chaos cells. The CI simulation leg remains the one unproven cell.

## Verdict

**SHIPPABLE (behaviour) with two P3 usability/trust defects around identity.** The messaging
contract — register, deliver, idempotent redelivery, lease, ack, drain — works as documented, is
fast, and is honest about wire encoding. The rough edge is what a user sees when their bridge
identity drifts: a reassuring "all 23 tools available" line next to a 401 wall, and a
"different public key" error for a key that does not exist.

## Left behind

- Rows `DF-CRIER-503`, `DF-CRIER-504` (pending, P3) — appended with `boardctl create`;
  `boardctl validate` → `RESULT: OK (447 warnings, 0 errors)`.
- This report, `diagnostics-2026-11-12-mcp-restart-identity.md`, the raw QA evidence copy, and a
  `skills/crier-usage/SKILL.md` section for the restart trap.
- One `dogfood-log.md` line.
- Local docker stack `crierdog` left **running** on ports 39501/39502: `docker compose down -v`
  is a destructive command and an unattended tick has no consent to run it. Reap manually.
