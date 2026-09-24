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
2026-09-24 | SHIPPABLE (behaviour) / docs-rough (xxd still) | 95s t2fs cold on fresh Debian 13 (28s Go + 38s build + signed round-trip), 3s mesh demo, <1s inbox demo | friction 3 | 3 findings (DF-CRIER-283 stale-pidfile takeover window + kill-JSON-pidfile, DF-CRIER-284 GOPATH collision, DF-CRIER-280 reproduced) — angle: durable-inbox contract (deliver/lease/ack batches), Postgres durability via docs compose recipe (21/21 survive 2 restarts), restart-survival contrast memory-vs-pg, cross-agent authz negatives (403/401/404), MCP stdio 13 tools; no PERF row (signed ops 8-30ms wall = curl+openssl-signing process forks, server 20-270µs per 09-23); bunker install real on las-bunker-03 agent 9ef9da05 @ ca28523, destroyed+verified
