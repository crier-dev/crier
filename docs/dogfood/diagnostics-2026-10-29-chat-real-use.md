# Diagnostics — crier-dogfood-2026-10-29-12-59-20

Target HEAD: ca27c8554ead58d0219f20bf0fec6db27b0e6857
Scratch port: 18799

Observed startup: `/health` 200 `{"status":"ok"}`; sqlite registry/session stores durable; auth disabled and agent signatures disabled deliberately for the local browser leg; guard disabled. The server logged build `v0.2.0-215-gca27c855-ca27c855-dirty` because the local build tree carried untracked residue.

Browser evidence:
- `/chat` served 200 text/html and hydrated as `🐴 Crier — Conversations`.
- Initial channel creation before applying the acting ID was refused in-page with `Apply an existing agent ID first. This is attribution, not authentication.`
- After applying alice, channel `583142c7521d09e842612cc1` was created.
- Bob was added with context summary and boundary message `0cf5732afa7679207fd3ff4b`.
- Message and task sends returned accepted receipts for bob. The transcript rendered `Addressed · no execution`; tasks rendered `open (recorded at send time, not live)`.
- Independent HTTP readback showed 3 messages, 2 open tasks, 2 participants, and dispatch evidence for all 3 sends.

Timing evidence:
- `/sessions` curl command returned HTTP 200 and rc 0 for 12 runs: p50 9.417 ms, min 7.699 ms, max 11.880 ms.
- Browser navigation API: initial 112.9 ms; reload 34.4 ms.
- Reload resource timings: session/status calls 5.7–9.4 ms.
- No user-noticeable performance finding; no profile and no PERF row.

Bunker evidence:
- Launch: `bunker-qa.sh launch /home/kara/crier`, agent b51af131, bunker-mvp, recorded HEAD ca27c855.
- Agent evidence was harvested before teardown from `$HOME/qa-evidence.jsonl` and then collected into `/tmp/bunker-qa-evidence-20261029T131048Z-1602990.jsonl`.
- fresh-install OK: `bin/crier`, 34,411,810 bytes.
- native suite PASS; upgrade reinstall OK; UI probe OK for `/chat` and `/docs`.
- ci-act UNVERIFIED: act failed setup of Go 1.26.9 due to a malformed `version` command invocation in the action environment; not graded as a product failure.
- chaos-disconnect INFO: live dependency unavailable under the network cut.
- chaos-resource INFO: expected 3 GiB virtual-memory cap OOM on the heavy suite.
- docker-deploy and chaos-shutdown SKIPPED by the controller safety gate; chaos-corruption N/A because the repo has no state files.
- Collect appended its row and destroyed the agent. `bunker info b51af131` returned `not_found`; the agent was absent from the subsequent census.

Teardown: local process stopped; port 18799 had no listener. Pre-existing unrelated untracked files were left untouched.
