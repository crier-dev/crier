2026-09-12 | PROMISING-BUT-ROUGH | 22s t2fs | friction 12 | 5 findings

2026-09-12 | PROMISING-BUT-ROUGH | 67s t2fs | friction 6 | 5 findings\n
2026-09-13 | PROMISING-BUT-ROUGH | 75s t2fs | friction 14 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 35s t2fs | friction 10 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 25s t2fs | friction 5 | 4 findings
2026-09-13 | PROMISING-BUT-ROUGH | 25s t2fs | friction 5 | 4 findings
2026-09-13 | PROMISING-BUT-ROUGH | 25s t2fs | friction 5 | 4 findings
2026-09-13 | PROMISING-BUT-ROUGH | 31s t2fs | friction 5 | 5 findings
2026-09-13 | PROMISING-BUT-ROUGH | 75.172s t2fs | friction 7 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 23.7s t2fs | friction 5 | 5 findings
2026-09-13 | PROMISING-BUT-ROUGH | 20.9s t2fs | friction 4 | 5 findings
2026-09-13 | PROMISING-BUT-ROUGH | 13.91s t2fs | friction 9 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 0.898s t2fs | friction 8 | 5 findings

2026-09-13 | UNKNOWN-VALUE | n/a t2fs | friction 0 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 38s t2fs | friction 5 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 1.02s t2fs | friction 15 | 5 findings\n
2026-09-13 | PROMISING-BUT-ROUGH | 1.001s t2fs | friction 12 | 5 findings
2026-09-13 | PROMISING-BUT-ROUGH | 1.13s t2fs | friction 12 | 5 findings

2026-09-13 | PROMISING-BUT-ROUGH | 1s t2fs | friction 9 | 5 findings
2026-09-13 | PROMISING-BUT-ROUGH | n/a t2fs | friction 0 | 5 findings
2026-09-13 | PROMISING-BUT-ROUGH | 1s t2fs | friction 8 | 5 findings\n
2026-09-14 | PROMISING-BUT-ROUGH | n/a t2fs | friction 0 | 4 findings

2026-09-14 | PROMISING-BUT-ROUGH | 1.008s t2fs | friction 10 | 5 findings\n
2026-09-14 | PROMISING-BUT-ROUGH | n/a t2fs | friction 0 | 5 findings\n
2026-09-14 | SHIPPABLE | 58s t2fs (fresh-machine incl. toolchain; ~5s on warm host) | friction 3 | 3 findings (DF-CRIER-129/130/131)
2026-09-14 | UNKNOWN-VALUE | n/a t2fs | friction 0 | 4 findings

2026-09-14 | PROMISING-BUT-ROUGH | 1.853s t2fs | friction 14 | 5 findings\n
2026-09-14 | PROMISING-BUT-ROUGH | n/a t2fs | friction 0 | 4 findings\n
2026-09-14 (2nd) | SHIPPABLE | 4.2s t2fs (build 3.1s + health; guard verdicts ~1.1-1.7s) | friction 4 | 4 findings (DF-CRIER-147/148/149/150) — first live-LLM guard run; bunker leg rerun at 0bd0fc7 (35s, new Makefile, smoke ok)
2026-09-15 | PROMISING-BUT-ROUGH | 0.97s t2fs | friction 14 | 5 findings

2026-09-15 | PROMISING-BUT-ROUGH | 30s t2fs | friction 12 | 5 findings
2026-09-15 | UNKNOWN-VALUE | n/a t2fs | friction 0 | 4 findings

2026-09-15 | PROMISING-BUT-ROUGH | 1.01s t2fs | friction 14 | 5 findings
2026-09-15 | UNKNOWN-VALUE | 2.6s t2fs | friction 28 | 0 findings

2026-09-16 | PROMISING-BUT-ROUGH | 16s t2fs | friction 9 | 5 findings
2026-09-16 | PROMISING-BUT-ROUGH | 8s t2fs | friction 11 | 5 findings
2026-09-16 | PROMISING-BUT-ROUGH | 12s t2fs | friction 10 | 5 findings

2026-09-18 | PROMISING-BUT-ROUGH | ~90s t2fs (build 1.3s + health + register) | friction 5 | 4 findings (DOGFOOD-RELAY-1/4, DOGFOOD-MESH-2/3) — real-use relay+mesh+MCP run; bunker install 66s, smoke ok, agent destroyed
2026-09-19 | PROMISING-BUT-ROUGH (trending SHIPPABLE) | ~90s t2fs | friction 2 | 2 findings (DF-CRIER-262 signed-PATCH README gap, DF-CRIER-263 SKIPPED-install-bunker: bunkerd kills rootless-docker installer at ~30s spawn deadline, docker.service already active — infra defect, not crier) — full real-use run: signed inbox+lease+ack, relay WS pub/sub, blocking webhook w/ reply, postgres restart durability (14 agents survived), MCP remote store E2E; rows verified on board tail
2026-09-23 | PROMISING-BUT-ROUGH → SHIPPABLE (behaviour) / ROUGH (install docs) | ~90s t2fs on fresh Debian 13 (9s Go install + 35s build + full signed round-trip), 180ms warm | friction 5 | 4 findings (DF-CRIER-279 payload.text template delivers EMPTY body silently, DF-CRIER-280 xxd has no non-root path and demo.sh hard-refuses, DF-CRIER-281 CR-GAP-069 ask #2 never implemented / 20 dogfood containers + 14 ports live, DF-CRIER-282 recovered delivery's reply dropped, spec silent) — real-use run: federation hold/recovery live (recovery forward watched for the first time), raw-mesh client (20 rtt each way 0.257/0.262ms), relay hop 1.288ms, multi-endpoint workflow, no PERF row (server 20-270µs/handler; 8ms hyperfine is curl startup); bunker install leg real on las-bunker-03 agent 4071c51f, destroyed after; auth-exempt surface re-measured live for CR-GAP-063
2026-09-24 | SHIPPABLE (behaviour) / docs-rough (xxd still) | 95s t2fs cold on fresh Debian 13 (28s Go + 38s build + signed round-trip), 3s mesh demo, <1s inbox demo | friction 3 | 3 findings (DF-CRIER-283 stale-pidfile takeover window + kill-JSON-pidfile, DF-CRIER-284 GOPATH collision, DF-CRIER-280 reproduced) — angle: durable-inbox contract; Postgres durability via docs compose recipe (21/21 survive 2 restarts); restart-survival contrast; cross-agent authz negatives; MCP stdio 13 tools; no PERF row; bunker install real on las-bunker-03 agent 9ef9da05 @ ca28523, destroyed+verified
2026-09-25 | SHIPPABLE (behaviour) | fresh install leg real (Go dl + make build 38s + signed round-trip 201/201/200/204 @ b0c9afa; destroyed) | friction 2 | 2 findings (DF-CRIER-285 bunker-qa launch crash; DF-CRIER-286 /docs says "other four" listing five) — rate limiter, /docs + /openapi.json, registry PATCH liveness, DF-CRIER-280 reproduced; no PERF row
2026-10-01 | SHIPPABLE (behaviour) / docs-rough | t2fs covered by bunker leg; warm ops below user-noticeable threshold; 2 findings DF-CRIER-295/296; install real agent 1fc609f5, destroyed+verified
2026-10-09 | SHIPPABLE (behaviour) / docs-blocked DF-CRIER-297 | ACL matrix live; findings DF-CRIER-297..302; no PERF row; install real agent 1266c7c7 destroyed+verified
2026-10-10 | SHIPPABLE (behaviour) / one hard boot trap | sqlite chat/session workflow live; findings DF-CRIER-303..308; no PERF row; install real agent f29aea56 destroyed+verified
2026-10-13 | PROMISING-BUT-ROUGH / one P1 wiring gap | federation peer-auth and policy walls live; session fan-out wiring gap; findings DF-CRIER-309..312; install explicitly skipped after prior fresh-box proof
2026-10-13 | SHIPPABLE (behaviour) / task-authority doc gap | task lifecycle re-proof live; findings DF-CRIER-314..318; no PERF row; install real and destroyed
2026-10-16 | SHIPPABLE (behaviour) / one P1 residual | webhook failure ladder live; DF-CRIER-320..322; no PERF row; install not run this tick, explicitly carried prior proof
2026-10-16 | SHIPPABLE (behaviour) | router hints + chat UI live; DF-CRIER-323..326; no PERF row; install real and destroyed
2026-10-20 | SHIPPABLE (behaviour) | board retention gate re-proof plus DF-CRIER-427; install real and destroyed
2026-10-20b | SHIPPABLE (behaviour) | real git hook/board retention controls; no new defect; no PERF row
2026-10-20c | SHIPPABLE (behaviour) | real Python consumer durable workflow; DF-CRIER-435; no PERF row
2026-10-23 | SHIPPABLE (behaviour) / one silent-wire P1 | groups/context/lifecycle live; findings DF-CRIER-475..479; no PERF row; install real and destroyed
2026-10-23 (run 2) | SHIPPABLE (behaviour) / root-caused INT-CRIER-015 | context and chat delta re-proof; findings DF-CRIER-480..483; no PERF row; install used pre-existing crier-lab and was not destroyed
2026-10-29 | SHIPPABLE (browser chat path) / install battery PARTIAL | target HEAD ca27c855 | real browser `/chat`: create channel → add bob with context summary → addressed message → two explicit tasks; readback 3 messages, 2 open tasks, 2 participants, accepted receipts and dispatch evidence | friction 1 expected identity-application step; UI correctly labels accepted inbox work as `Addressed · no execution` and task state as send-time, not live execution | PERF no row: `/sessions` curl p50 9.417ms across 12 runs (min 7.699, max 11.880), browser initial 112.9ms/reload 34.4ms | bunker-qa launch/collect real on bunker-mvp agent b51af131: fresh-install OK, native suite PASS, upgrade OK, UI probe OK; ci-act UNVERIFIED, chaos-disconnect/resource INFO, controller Docker lifecycle SKIPPED; evidence `/tmp/bunker-qa-evidence-20261029T131048Z-1602990.jsonl`, agent destroyed and `info` returned not_found | no new finding row; browser-auth owner remains DF-CRIER-333; no board mutation | artifacts `docs/dogfood/2026-10-29-chat-real-use.md`, `docs/dogfood/diagnostics-2026-10-29-chat-real-use.md`, `skills/crier-usage/SKILL.md` | scratch port 18799 stopped and verified free; unrelated untracked residue untouched

2026-10-29 | SHIPPABLE (A2A core) / install battery PARTIAL | target HEAD ca27c855 | real A2A client: opt-in agent -> Agent Card -> SendMessage -> exact messageId retry -> GetTask/ListTasks; same task id and replay=new/replayed, durable SQLite; friction 0 product defects | PERF no row: warm SendMessage 12.3ms +/-0.6 n=20 (11.1-13.1), cold fresh-store boot+register+send+stop 197.9ms +/-6.4 n=5; below user-noticeable threshold | bunker-qa real launch/collect on bunker-mvp agent 046824f2: fresh-install/upgrade/UI OK, native ci-pass rc=1 plus act setup-go harness error, chaos INFO, controller Docker SKIPPED; collect OK rows=20 and post-destroy census absent | no new finding row; existing QA-CRIER-48/QA-CRIER-7 owns act harness class; artifacts docs/dogfood/2026-10-29-a2a-real-use.md + diagnostics-2026-10-29-a2a-real-use.md; no board mutation
