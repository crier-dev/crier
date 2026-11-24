# Evidence index — crier-dogfood-2026-11-24-19-47-57

Real-use run of the first-party client surface (`clients/README.md` "The whole
thing, from nothing") plus a restart-persistence trust arm, tree `b48e1aad`
(= origin/main at fire time). Everything was produced on the scratch tree
`/tmp/dogfood-crier/` on a clean data home; no production data touched.

## Probe scripts (these produced the numbers; ruff-formatted before commit —
## disclose: not byte-identical to the runs, output text unchanged)

| file | what it did |
|---|---|
| `stepA.sh` | documented `make build`, timed; binary identity check |
| `stepBCD.sh` | `crier keygen` ×3 (timed), `docker compose up -d postgres` on an override port (5437 was held), server cold boot → `/health` 200, `GET /status` |
| `stepE.sh` | the documented `round_trip.py` first attempt → **409 public_key_mismatch** (stale-volume tombstone, DF-CRIER-515); also `GET /agents` showing 2026-09-30 residue |
| `stepE2.sh` | `sign_delete.py` + signed `DELETE /agents/alice` with the NEW key → **401 signature verification failed** (the 409 remedy is unexecutable); re-ran the documented round trip under a fresh id → green |
| `stepF.sh` | documented `two_agents.py` two-identity journey (403/401 negative controls) |
| `durable_queue.py` | scratch durable work-queue consumer: produce → ack 15/20 → restart → survive-verify (v1 asserted against a confounded queue; superseded by v2 below; kept for the record) |
| `durability_clean.py` | clean-slate v2 arm: deliver 10, ack 7 (for real), wait the 30 s lease, restart, verify exactly the 3 unacked survive with payloads intact |
| `probe_queue2.py` | the interim probe that showed retrieve leases its whole batch (returns 0 while leases live) |
| `sign_delete.py` | signed-request trio for `DELETE /agents/{id}` (no body) using the client's transcript format |
| `stepG.sh` / `stepH-prep.sh` / `stepI.sh` | stop/restart orchestration and the clean re-run |
| `stepJ-file.sh` | the boardctl row filing for DF-CRIER-515 / DF-CRIER-516 |

## Captures (verbatim product output)

| file | content |
|---|---|
| `rt.err` / `rt.out` | the 409 first-contact failure, then the green 12-step round trip under the fresh id |
| `del.json` | the 401 from DELETE signed with the new key |
| `agents.json` | `GET /agents` at fire time: 2026-09-30 registrations, `status: stale` |
| `ta.out` | two_agents journey output |
| `ts.out` | TypeScript round-trip output (12 steps green) |
| `produce.log` | durable-queue produce run with per-op LAT lines (deliver mean 5.69 ms, ack 6.75 ms, n=20/15) |
| `server.log`, `server2.log` | server boot logs (cold boot; restart) |
| `status.json`, `health.json` | `/status` (`registry_backend: postgres`, durable) and `/health` |
| `build.time`, `keygen.time`, `rt.time`, `ta.time` | /usr/bin/time wall figures |

## NOT committed, deliberately

- `alice.key` / `bob.key` / `worker.key` / etc.: scratch ed25519 private keys —
  throwaway test keypairs for throwaway ids; they sign nothing real and their
  public halves are visible in `agents.json`/transcript output only.
- Nothing from any production bus, log or user data — the whole run was a
  scratch server on localhost:18876.

## Battery (install leg) — COMPLETED, 24 cells, grade GOOD-PARTIAL

`bunker-qa.sh launch /home/kara/crier --keep` against `bunker-las-03`
(BUNKER_QA_SERVER override; default host bunker-mvp preflight-FAILED at 95 %
disk vs 94 % threshold — that refusal is in the run record, not graded as a
product failure). Agent `a9967ba5`, fresh-install cell from an empty module
cache; run finished (`qa-run.finished`, log tail `CELLS-DONE`); ledger
collected ONCE at close into `.coding-hermes/evidence/qa-20261124T202454Z/`
(EVIDENCE-SHA256 b6ec89b042e9…d5c23b, 24 product rows / 28-file capture) with
per-cell logs under `logs/qa-logs-2054014/`.

Cell outcomes: OK — launch, toolchain-bootstrap ×9, fresh-install (real `go
build` from an empty cache), lint, compose_config, docker_build, ui-probe,
upgrade, chaos-errorpath (clean documented failure on broken config), 
chaos-disconnect; FAIL — tests (DF-CRIER-517: DOCTOR_PROBE_TIMEOUT=5 claim
bound exceeded on the fresh machine) and ci-pass (DF-CRIER-518: pid-identity
race assertion in TestStopRefusesForeignProcess — the product's refusal itself
held); UNVERIFIED — ci-act (act infra), chaos-resource (harness 3G ulimit cap
artifact, by the ledger's own text); SKIPPED — docker-deploy + chaos-shutdown
(safety gate: controller is the live Hermes host); WARN evidence-outside-repo
(resolves here: the ledger is committed in-repo). Agent destroyed after
collection (bunker list — 0 hits for the id).
