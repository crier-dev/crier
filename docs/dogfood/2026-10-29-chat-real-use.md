# Crier chat real-use — 2026-10-29

Tick: crier-dogfood-2026-10-29-12-59-20
Target: ca27c8554ead58d0219f20bf0fec6db27b0e6857

## Promise

A real operator can use the embedded browser client to create a channel, add a registered agent, send a message, create a task, and read back the resulting transcript and task state.

## Live browser evidence

Scratch server: `CR_REGISTRY_BACKEND=sqlite CR_SESSION_BACKEND=sqlite CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false ./bin/crier -port 18799`, `/health` returned `{"status":"ok"}`. The browser loaded `http://127.0.0.1:18799/chat` as `🐴 Crier — Conversations`.

The browser created channel `583142c7521d09e842612cc1`, added `bob` as an agent with summary context, sent `hello from the browser dogfood`, and created two explicit tasks for `bob`. The UI rendered the message as `Addressed · no execution`, showed `bob: accepted`, and rendered task state `open (recorded at send time, not live)`. Server readback of `/sessions/583142c7521d09e842612cc1/messages` contained 3 messages, 2 task messages, dispatch evidence, and accepted receipts; `/tasks` contained 2 open tasks; `/participants` contained alice and bob.

The first channel creation attempt, before applying the acting ID, was correctly refused in the UI with `Apply an existing agent ID first. This is attribution, not authentication.` Applying `alice` then allowed the create flow. This is expected client guidance, not a finding.

## Performance evidence

Headline command: `curl -sS -o /tmp/crier-sessions-body -w '%{http_code}' http://127.0.0.1:18799/sessions`, rc 0/http 200, 12 runs: p50 9.417 ms, min 7.699 ms, max 11.880 ms. Browser navigation: initial 112.9 ms and reload 34.4 ms; measured session/status resource requests on reload were 5.7–9.4 ms. No user-noticeable cost and no PERF row.

## Limits

The browser run used a deliberate local-dev posture with auth and agent signatures disabled, so it does not prove the authenticated browser arrangement. That known owner decision remains DF-CRIER-333. Accepted inbox receipts do not prove recipient execution or reply; the UI correctly says so. The browser build banner identified commit ca27c855 but marked the binary modified due to local residue, so this is current-HEAD corroboration rather than a clean artifact claim.

## Install leg

`bunker-qa.sh launch /home/kara/crier` then `collect --evidence /tmp/bunker-qa-evidence-20261029T131048Z-1602990.jsonl` ran on bunker-mvp agent b51af131. Agent evidence was harvested before teardown. Fresh install was OK with `bin/crier` (34411810 bytes); native suite PASS; upgrade reinstall OK; UI probe OK. `ci-act` was UNVERIFIED because act failed to set up Go 1.26.9; chaos-disconnect was INFO under a blocked dependency; chaos-resource was INFO under the expected 3 GiB cap; controller safety skipped Docker lifecycle cells. Collect appended its row and destroyed the agent; `bunker info b51af131` returned not_found afterward.

No new board row was filed. This read-only lane does not mint IDs or mutate board state.
