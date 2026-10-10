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

2026-09-25 | SHIPPABLE (behaviour) | t2fs covered by 09-24 fresh-box run; this run: fresh install leg real (Go dl + make build 38s + signed round-trip 201/201/200/204 @ b0c9afa on las-bunker-03 agent b1ff39dc, destroyed) | friction 2 | 2 findings (DF-CRIER-285 bunker-qa launch crash in upgrade-prep — fleet tool, battery never ran, manual install leg substituted; DF-CRIER-286 /docs says "other four" listing five) — angle: rate limiter (100x202 then 429, per-agent isolation, window reset), publish signature presence-only by design (claims gate STATUS-README-RELAY-PUBLISH-BOGUS-SIG-202, trust note in integration report, NOT a bug), /docs + /openapi.json live contract (17-20ms), registry PATCH liveness contract (last_seen moves only on signed PATCH; wrong-key 401, stale-ts 401 ±30s), DF-CRIER-280 reproduced independently (bare Debian no xxd, od substitution works); no PERF row (67ms/publish = curl+openssl forks, server 20-270µs per 09-23; docs/openapi 17-20ms)
2026-09-25 (2nd run, A2A-gate angle per the change-the-angle rule) | SHIPPABLE (behaviour) | no PERF row (cold/restart-to-healthy 119ms, warm deliver+signed retrieve+ack 55-64ms via curl incl signing forks, server p50 5.6ms - nothing user-noticeable) | friction 3 | 2 rows (DF-CRIER-288 README silent on 409 duplicate register; DF-CRIER-289 SKIPPED-install-bunker: las-03 + las-01 offline per tailscale, las-02 bunkerd crash-loop NRestarts=24998, no spawn possible on any host - crier installability rests on the same-day earlier fresh-box pass @ b0c9afa) - INT-A2A-001 FIRST real-use evidence (merged same day): strict decode names offending+accepted key and registers NOTHING, wrong-type = generic 400 (documented webhook parity), a2a:null clears (three-state), {} round-trips, opt-in + {} + keyless shapes all survive PG restart (migration 005), pre-restart lease survives restart, openapi.json md5-identical across unset/on/restart, no A2A route under either switch value, ordinary messaging green on opted-in agents both postures; artifacts docs/dogfood/2026-09-25-a2a-gate-real-use.md + diagnostics-2026-09-25b-a2a-gate.md + skills/crier-usage A2A section; scratch stack destroyed after

2026-10-01 | SHIPPABLE (behaviour) / docs-rough (demo.sh xxd) | t2fs covered by bunker leg (Go dl + build 31s + register 201/deliver 201/publish 202 via binary keygen path); warm ops: health 5ms, deliver w/ live guard 0.8-1.4s (bounded by deepseek RTT 1.4-1.6s, NOT server), status 7ms | friction 3 | 2 findings (DF-CRIER-295 signed-HTTP/WS wire-format drift undocumented in openapi; DF-CRIER-296 mesh direct-send key + silent registry "no liveness evidence" on non-mesh agent) — angle: fresh-real-use of the 09-29 fix wave at HEAD 649d9e3 (CR-REVIEW-002 REGISTER_ACK: verified correlated acks on both peers incl. no agent_id/payload fields; QA-CRIER-35 relay WS keepalive: 35s-idle wildcard subscribe still received publish, socket alive; CR-GAP-072 RemoteStore 404 keying: MCP bridge get_agent/unregister on ghost names "agent not found" subject-correct); mesh RPC full round-trip (correlated RESPONSE status 200 body relay), ERROR frames carry request_id (CONTROLLER_OFFLINE + INVALID_MESSAGE), INBOX_NOTIFY opt-in verified matching message id + sender, signed HTTP round-trip deliver→retrieve(6 msgs)→ack 204→stats 0, wrong-key 401, stale-ts 401, restart durability (agents+messages survived), guard live with DEEPSEEK key (clean allow, no guard obj on clean pass) vs fail-open errored:true without key; MCP stdio 13 tools, mesh_request honestly reported unavailable without CRIER_MESH_URL; PERF: no PERF row — nothing user-noticeable beyond guard's provider RTT (documented); install leg REAL on las-bunker-03 agent 1fc609f5 (bundle-based clone, network fetch denied by host key no-credentials — per-file rule noted), build 31s, smoke ok, agent destroyed+verified

2026-10-09 | SHIPPABLE (behaviour) / docs-blocked (DF-CRIER-297) | angle: CR-CHAT-007 human-identity phase 1 at 8c316e3b (fresh product change since 10-01) — principals/bindings/agent-class/grants + delivery ACL armed and exercised as a two-owner deployment | ACL matrix verified live: anonymous-on-classed 403 ANONYMOUS, cross-owner 403 NO_GRANT naming principal/as_agent/target/owner, no-binding 403 NO_BINDING, viewer-send denied (binding≠send), owner→same-realm service allowed, grant flips 403→201, revoke tombstone + past expires_at both inert next request, capability pool resolves holder then ACL-checks the RESOLVED agent (T4 closed, ANONYMOUS at the address on action invoke), class records + bindings + grants survive restart (JSONL), zero-timestamp + 500-on-bad-role + ghost-binding accepted filed as DF-CRIER-300/301 | friction 3 (DF-CRIER-297 P0 dual-token deadlock makes the documented production posture unreachable — Bearer cannot equal two secrets; DF-CRIER-298 admin token undocumented; DF-CRIER-299 class route missing from openapi) + DF-CRIER-302 (409 key immutability) | PERF: no row — delivery 8.5ms±0.5 warm / refusal 8.1ms (curl-fork dominated, server 0.2-0.5ms per access log), boot-to-healthy 80-108ms n=5, nothing user-noticeable | install leg REAL on bunker-las-02 agent 1266c7c7 (spawn retried once after transient slice-limits; https clone ok, Go tarball on bare Debian + make build 67s, smoke health/mint/register/class all 2xx, agent destroyed+verified) | artifacts docs/dogfood/2026-10-09-delivery-acl-real-use.md + diagnostics-2026-10-09-delivery-acl.md + crier-usage SKILL ACL section; rows DF-CRIER-297..302 committed 551e10e

2026-10-10 | SHIPPABLE (behaviour) / one hard boot trap | angle: the REST of the CR-CHAT wave at 8c316e3 (10-09 run covered CR-CHAT-007 ACL only; this one: sqlite backend, sessions/messages/threads, task lifecycle, named groups, compile+output modes, dagger control, keygen->sign->retrieve as a fresh integrator) | full round trip proven live: sqlite-only boot (no pg, no cgo), session 201, participants, payload/sender messages with parent_id threading (seq-ordered transcript), group create + @team fan-out (after agent registration — refusals before), signed retrieve (openssl oneshot needs -in FILE) -> ack 204 -> stats 0, restart durability (session+3 msgs+task+group intact), compile summary covered 2 threads/3 msgs, dagger 502/404 honest error contract | findings DF-CRIER-303 (P1: CR_DAGGER_URL w/o CR_DAGGER_STORE_DIR = hard boot fail on non-root, /var/lib/crier mkdir denied, reproduced control-host AND fresh bunker) + DF-CRIER-304 (P2 task lifecycle unusable past creation: no read route, claim/complete TASK_NOT_FOUND on just-created task) + DF-CRIER-305 (P2 group fan-out requires registered agents, undocumented) + DF-CRIER-306 (P3 keygen key + openssl oneshot signing recipe undocumented) + DF-CRIER-307 (P2 INSTALL-crier REAL on bunker-mvp agent f29aea56: gitlab https clone denied private repo -> tar substitute named as such, go build 94s incl toolchain dl, health+session smoke ok, agent destroyed+verified) + DF-CRIER-308 (P3 perf record: health 2-16ms, create 25-110ms, transcript 0.5-18ms, cold start ~1.2s — nothing user-noticeable, no PERF row) | friction 7 | time-to-first-success ~10min (request-shape walls: created_by object, payload/sender not from, group name bare) | artifacts docs/dogfood/2026-10-10-chat-crchat-wave.md + diagnostics-2026-10-10-crchat-wave.md + crier-usage SKILL sqlite/chat-session section
