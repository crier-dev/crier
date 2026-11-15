# crier real-use report — inbox consumer contract + install leg (2026-11-15)

Run: `crier-dogfood-2026-11-15-15-54-49` (dogfood lane, skill-driven, no driver script).
Target: `crier` at `/home/kara/crier`, HEAD `9d0240f4` (== `origin/main`, `rev-list --left-right --count HEAD...origin/main` = `0 0`).
Host: `karaHermes-mde-7840hs`, binary `crier v0.6.1-228-g9d0240f4`.
Scratch: `~/.hermes/cache/scratch/crier-dogfood-20261115-155449/` (`probe.sh`, `consumer-e2e.sh`, `e2e2.sh`, `e2e3.sh`, `perf.sh`, `*.out`).

## Verdict

SHIPPABLE for the consumer inbox contract. One real-use journey — register two agents,
long-poll for a message, ack it, retrieve/ack a batch, let a short-TTL message expire
unacked, watch the expiry sweep dead-letter it and write the documented receipt to the
sender's inbox — passed end to end, with the receipt matching README L126 field for field.
The only red surface is the harness's own `act` CI simulation, filed as a recurrence
(see Findings). Nothing was slow enough for a user to feel.

## The journey, as a consumer runs it

Server: `CR_GUARD_ENABLED=false CR_REQUIRE_AGENT_SIG=false ./bin/crier -port 8891`
(the documented local/dev mode; no credential minted).

```
cold start (exec -> /health 200): 71ms
register consumer worker-b (=201)            PASS
register sender  boss-b  (=201)              PASS
long-poll: parked read returned in 0.633s (deliver landed at ~0.5s), messages=1   PASS
ack returns 204 per spec                     PASS
retrieve claims all 3 queued messages (=3)   PASS
ack 2 of 3 returns 204                       PASS
stats: queue_depth=1 after acking 2 (=1)     PASS
expired unacked message is dead-lettered      PASS
sender got a MESSAGE_EXPIRED receipt          PASS  (verified decoded, see below)
```

`GET /agents/worker-b/inbox/stats` verbatim after the partial ack:

```json
{"queue_depth":1,"leased_count":1,"oldest_age_ms":700,
 "oldest_unacked":"2026-11-15T17:02:36.292202208Z",
 "contract_ttl_secs":86400,"stale":false,"unacked_count":1}
```

The expiry sweep (ttl 3s, message claimed but never acked) produced, in the SENDER's
inbox, exactly the object README L126 promises:

```json
{"kind":"error","code":"MESSAGE_EXPIRED","message_id":"0ff391b204c58545bceb29ee",
 "target":"worker-c","sender":"boss-c",
 "expires_at":"2026-11-15T17:04:02.870292156Z",
 "dead_lettered_at":"2026-11-15T17:04:29.826722696Z","dead_lettered":true,
 "dead_letter_path":"/agents/worker-c/inbox/dead-letters",
 "reason":"ttl_expired_unacked"}
```

and the matching dead letter:

```json
{"message_id":"0ff391b204c58545bceb29ee","agent_id":"worker-c","sender":"boss-c",
 "created_at":"2026-11-15T17:03:59.870292156Z","expires_at":"2026-11-15T17:04:02.870292156Z",
 "dead_lettered_at":"2026-11-15T17:04:29.826722696Z","reason":"ttl_expired_unacked"}
```

Sweep latency: message expired at `17:04:02.870Z`, receipt written `17:04:29.826Z` — the
sweep interval, not a per-message cost.

### Two false FAILs of my own making (recorded so the next consumer does not repeat them)

The first run of this journey reported 4 FAILs; 3 were harness bugs, 1 was a projection bug:

1. `ack` was expected to answer `200` — the spec and `skills/crier-usage/SKILL.md:54,482`
   both say **204** (no body). The server was right.
2. `stats` was compared as a JSON substring (`'"queue_depth":1'`) against a
   compact-ordered object. Compare the field, not the text.
3. The receipt was searched for `kind == "MESSAGE_EXPIRED"` — the documented discriminator
   is **`code`**, while `kind` is always `error` (the same shape as `WEBHOOK_FAILED` /
   `FEDERATION_FAILED`). Decoding via `.payload | @base64d | fromjson` and reading `.code`
   makes the check pass.

## Performance (Step 2b)

Numbers, not adjectives; the point is whether a user would feel anything.

| operation | measurement |
|---|---|
| cold boot (exec → `/health` 200), n=5 | 43, 47, 50, 49, 48 ms (mean 47.4); a later run 57 ms and 71 ms |
| deliver (HTTP round trip incl. `curl` spawn), n=30 | p50 11.60 ms, p95 12.58 ms, max 13.27 ms |
| retrieve, n=30 | p50 11.78 ms, p95 12.87 ms, max 13.26 ms |
| ack, n=30 | p50 11.59 ms, p95 12.40 ms, max 12.73 ms |
| long-poll wake after a deliver lands mid-park | ~4 ms (returned 0.504 s with the deliver at 0.5 s); second run ~133 ms (0.633 s) |

The per-op figures are dominated by process spawn and HTTP overhead on a loaded host, and
every one of them is an order of magnitude below a user-noticeable threshold. **No PERF row
filed — there is nothing here a user could feel.**

## Findings

- **DF-CRIER-497-R2** (P2, pending) — recurrence of DF-CRIER-497 at a *new* step: the `act`
  CI simulation on a fresh clean agent fails at `Agent-ecosystem registration contract`
  (`go test -count=1 ./internal/ci`, 30.3 s, exit 1) with **zero step stdout captured**,
  which takes the whole `ci-pass` cell to FAIL. Attribution, deliberately not a product
  defect: hosted CI is green on the pin (`gh run list -w ci.yml`: success,
  `2026-11-15T13:26:07Z`, `9d0240f4`), and the same test passes locally in 0.143 s. Filed as
  a recurrence via the minter (`--recur DF-CRIER-497`), not a bare sibling.
- **DF-CRIER-498** (`docker-deploy` cell reports `compose up OK, probe 000`) was observed
  again — already migrated to the coding-hermes-tools board on an earlier fire, so it is
  cited, not re-filed.

## Install leg (fresh machine, REAL — not skipped)

Ephemeral bunker agent `765c30b7` on `bunker-las-03`, launched with
`bunker-qa.sh launch`, evidence harvested with `bunker exec` (the `collect` transport failed
its SSH probe — see below), agent destroyed afterwards: `Agent 765c30b7 destroyed`, home
7.9 GB reclaimed.

26 evidence rows; product cells:

- **OK**: `toolchain-bootstrap` ×9 (discovered go 1.27.4 at `~/tools/go`, floor 1.26.10;
  gcc/g++ wrappers; make 4.4.1; jq; node v22.23.4/npm 10.9.9; compose + buildx plugins),
  `fresh-install` (`go build ./...` from an empty module cache), `lint` (`make lint` rc=0),
  `tests` (`go test ./...` rc=0 after the whole suite incl. `internal/webhook` 16.8 s),
  `docker_build` (rc=0, context 107,798,848 B, buildx), `compose_config`, `upgrade`
  (previous release `v0.6.0` tree via `git archive` → HEAD reinstall clean), `ui-probe`,
  `chaos-shutdown`, `chaos-errorpath` (clean documented failure: `invalid CRIER_PORT:
  "definitely-not-a-port"` names the variable).
- **INFO**: `docker-deploy` (compose up OK, probe 000 — DF-CRIER-498), `chaos-disconnect`
  (passes with the network cut), `chaos-resource` (OOM under the harness's 3 GiB cap while
  `internal/webhook` runs).
- **N/A**: `chaos-corruption` (no db/state files in repo).
- **FAIL**: `ci-pass` (act rc=1 → DF-CRIER-497-R2); **UNVERIFIED**: `ci-act` (a failed act
  leg is never graded OK; hosted CI is authoritative).

### Install-leg transport gap (harness, disclosed not hidden)

`bunker-qa.sh collect --evidence …` aborted: `ssh probe returned no state` for the spawned
agent, although the agent was alive (7.9 GB home, `qa-run.finished` present, 26 evidence
rows) and `bunker list --server bunker-las-03 --status all` showed it running. The battery
itself completed at 16:33Z; the evidence above was harvested over `bunker exec` instead.
Whoever owns the collect path should treat the SSH probe as the fragile part — the run was
fine, only the pull-back was not.

## Left behind / teardown

- Bunker agent `765c30b7` **destroyed**; no agent residue on `bunker-las-03`.
- My two scratch servers (ports 8875, 8876, cwd = this tick's scratch dir, attributed via
  `/proc/<pid>/cwd` before the kill) **stopped**; `ss -lntp` shows 8875/8876 free.
- Untouched on purpose: `crierdog-crier-1` (39501) / `crierdog-postgres-1` (39502) and the
  `crier_dogfood_20261112` database from an earlier fire (`docker compose down -v` /
  `dropdb` are destructive and an unattended tick has no consent), plus the stale QA stacks
  `qa-crier-repro`, `qacrier1625`, `carter-review-crier`.
- Never touched: the production daemon `bin/crier -pidfile /home/kara/crier-fleet/crier.pid`
  (`pid 3203032`, listening on 8767, started Nov 8) — it is live, not scratch.
- Repo changes: `.coding-hermes/board/tasks.jsonl` (one row), `.coding-hermes/dogfood-log.md`
  (this fire's line), this report, and a `skills/crier-usage` section on expiry receipts. No
  product source file was modified.
