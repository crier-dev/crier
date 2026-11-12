# Diagnostics — crier MCP restart/identity run, 2026-11-12

Evidence trail for `docs/dogfood/2026-11-12-mcp-restart-identity-real-use.md`.
All probe scripts and raw outputs are in
`/home/kara/.hermes/cache/scratch/crier-dogfood-20261112/` (host scratch, not repo).

## 0. Target identity, before any claim

```
git -C /home/kara/crier fetch origin main
HEAD=1774dcd7 ; git rev-list --left-right --count HEAD...origin/main -> 0  1
git diff --stat HEAD origin/main
  .coding-hermes/board/events.jsonl | 1 +
  ops/crier-ops-2026-11-12-14-29-21.md | 67 +++
```

One commit ahead, docs/board only: product code under test **is** `origin/main`.

## 1. Methods

- Server built with `make build`, run on a scratch port; signature enforcement off
  (`CR_REQUIRE_AGENT_SIG=false`) — the documented local mode.
- MCP: stdio `crier` bridge via a minimal JSON-RPC client (`mcp_client.py`), one process per
  probe, env = `CRIER_HTTP_URL`, `CRIER_AGENT_ID`, `CRIER_MESH_URL`.
- HTTP: direct `urllib` for paging/burst (bypasses the tool layer to isolate server behaviour).

### A correction worth recording (probe bug, not a product bug)

The first workflow probe read `content` / `isError` at the top level of the JSON-RPC reply
instead of inside `reply["result"]`, so **every** tool call looked empty. That produced a false
"tools answer nothing" impression which was discarded after re-reading the envelope correctly
(`verify_workflow.py`). Lesson kept in the usage skill: unwrap `result` before judging a call.

## 2. End-to-end contract (probe `e2e_real.py`)

```
register_agent(peer)                          ok  public_key="" status=online
deliver_message(peer, {n:1})                  ok  message_id=...
deliver_message(peer, {n:2}, key-abc)         ok  message_id=A
deliver_message(peer, {n:2}, key-abc)         ok  message_id=A  queue_depth still 2   <- dedupe
inbox_stats(peer)                             queue_depth 2
retrieve_inbox(peer, max_messages=10)         2 messages, lease_id=<uuid>, queue_depth 2
ack_messages(peer, lease_id, ids)             acked: 2
inbox_stats(peer)                             queue_depth 0
```

Idempotency returned the *same* id and did not add a third row — the property consumers need for
at-least-once delivery.

## 3. Paging (probes `page_size.py`, `page_cap.py`, `deep_page.py`)

| probe | queue | request | returned |
|---|---|---|---|
| page_size | 25 | `?limit=100` | 25 |
| page_cap/plain | 25 | `?limit=100` | 25 |
| page_cap/leased | 25 | `?limit=100&lease_seconds=300` | 25 |
| page_cap/deep_leased | 175 | same | 100 |
| deep_page | 395 | same, twice | 100 then 100 |

A single earlier burst observation returned 10/391 with the identical URL; it did **not**
reproduce under any of the controlled depths above, so it is recorded as an anomaly and no row
was filed. Note the second `deep_page` call re-leased 100 while the first 100 were still leased
(`queue_depth` unchanged) — expected for expiring leases, not pursued further.

## 4. Restart identity trap (probe `identity_trap.py`, raw `identity_trap.json`)

Verbatim from the second bridge's stderr:

```
INFO  MCP tool surface: all 23 advertised tools are available in this mode mode=remote/bridge tools=23
ERROR bridge identity already exists on the server with a DIFFERENT public key — every signed request from this bridge will be rejected with 401 agent_id=mcp-agent key_source=ephemeral remedy="set CRIER_AGENT_PRIVATE_KEY_FILE to the private key whose public half is registered for \"mcp-agent\", or delete the stale agent server-side (DELETE /agents/mcp-agent) and restart, or point the bridge at a fresh in-memory server"
remote GET /agents/mcp-agent/inbox?lease=30&max=5: status 401: {"error":"signature verification failed"}
```

Run 1 (same env, fresh server) succeeded fully: `deliver_message` → `{"message_id": ...}`,
`retrieve_inbox` → message with base64 `payload` that decodes to the sent JSON.

Only the *retrieve* was attempted on run 2, so this run does **not** claim that writes still
succeed after the identity drift — an earlier draft of that sentence was cut for lack of
evidence. What is proven: the availability line and the 401 reality coexist, and the remedy text
is correct.

## 5. Keyless re-registration (probe `verify_workflow.py`, raw `verify_workflow.json`)

```
register_agent(id=verify-peer-...)  ok    {"id":..., "public_key":"", ...}
register_agent(id=verify-peer-...)  err   agent already registered with a different public key
```

Source: `internal/registry/store.go:477`
`ErrPublicKeyMismatch = errors.New("agent already registered with a different public key")` —
reached with both keys empty, so the message states a cause that cannot exist.

## 6. Docker path (host, outside the battery)

```
docker compose -p crierdog up -d --build   (CRIER_HOST_PORT=39501 CRIER_PG_HOST_PORT=39502)
 crierdog-postgres-1 Healthy ; crierdog-crier-1 Started
curl -m2 -o /dev/null -w '%{http_code}' localhost:39501/health  ->  200  (first poll)
```

Two earlier attempts failed with `Bind for :::15437/:25437 failed: port is already allocated` —
those ports are held by **leftover stacks from earlier crier QA runs on this host**
(`qa-crier-repro-*` 18767/15437, `qacrier1625-*` 28767/25437, `carter-review-crier-postgres-1`).
That is host hygiene debt, and it explains the battery's `docker-deploy` cell: the cell probed a
port the container was never published on, so `probe 000` is a harness-target issue reported as
INFO rather than FAIL. The compose file's declared port (`CRIER_HOST_PORT`, default 8767) works.

Teardown of this stack was **not** performed: `docker compose down -v` is destructive and the
unattended tick had no consent. Stack left running on 39501/39502 for manual reaping.

## 7. Install battery (fresh machine)

```
bash ~/.hermes/scripts/bunker-qa.sh launch  (agent be52bcfd, server bunker-mvp)
bash ~/.hermes/scripts/bunker-qa.sh collect --evidence /tmp/bunker-qa-evidence-20261112T141432Z-349996.jsonl
COLLECTED agent=be52bcfd rows=24
bunker destroy be52bcfd --server bunker-mvp  -> "Agent be52bcfd destroyed."
bunker list --status all | grep be52bcfd      -> ABSENT
```

Cell census (24): `launch` OK · `toolchain-bootstrap` OK ×9 (go 1.27.4 discovered, gcc/g++,
make, jq) · `fresh-install` OK · `lint` OK · `tests` OK · `docker_build` OK · `compose_config`
OK · `upgrade` OK · `ui-probe` OK · `chaos-shutdown` OK · `chaos-errorpath` OK ·
`chaos-disconnect` INFO (`passes with network cut`) · `chaos-resource` INFO
(`OOM under cap … cap=3145728KB`, child+subshell saw the cap; test `internal/webhook` 16.799s) ·
`chaos-corruption` N/A (no db files) · `docker-deploy` INFO (`compose up OK, probe 000`) ·
`ci-pass` FAIL · `ci-act` UNVERIFIED.

CI detail available in the evidence is only the act preamble (`git clone actions/setup-go@v6`
then `docker cp` then `node … setup/index.js`), truncated before the failing step; the record
therefore says **UNVERIFIED**, not "CI is broken". Duplicate fire of the 2026-11-06 class;
`QA-CRIER-48` already owns the act harness.

## 8. Files in the repo touched by this run

`docs/dogfood/2026-11-12-mcp-restart-identity-real-use.md` (new),
`docs/dogfood/diagnostics-2026-11-12-mcp-restart-identity.md` (new),
`docs/dogfood/crier-dogfood-2026-11-12-qa-evidence.jsonl` (new, 24 rows),
`skills/crier-usage/SKILL.md` (identity-trap section),
`.coding-hermes/dogfood-log.md` (one line),
`.coding-hermes/board/tasks.jsonl` + `events.jsonl` (rows `DF-CRIER-503`, `DF-CRIER-504` only).
No source file changed.
