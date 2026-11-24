#!/bin/bash
# File the two dogfood findings as board rows (ids minted via board_next_id.py, verified free).
cd /home/kara/crier || exit 9
B=.coding-hermes/board

boardctl -C $B create \
  --id DF-CRIER-515 \
  --title 'Lost-key registry tombstone: the 409 remedy (DELETE) is unexecutable and no operator recovery exists' \
  --type bug \
  --priority P1 \
  --complexity 3 \
  --reasoning 'Real-use hit, dogfood tick crier-dogfood-2026-11-24-19-47-57 on the documented from-nothing path (clients/README section The whole thing from nothing) against the durable postgres backend: the compose volume (crier_pgdata) carried agents registered 2026-09-30 by an earlier run whose key files no longer exist. crier keygen writes a NEW key for id alice; POST /agents answers 409 {"error":"public_key_mismatch","message":"existing service identity uses another key"} and the message says: delete the agent first (DELETE /agents/{id}) or register under a new id. DELETE /agents/alice signed with the NEW key answers 401 signature verification failed, because DELETE requires the signature of the agents REGISTERED key. Key immutability is by design (internal/registry/reregistration_test.go), but the recovery the error text names is impossible in exactly the situation the error occurs, and no operator path exists: the management surface writes (POST /principals /bindings /grants /grants/{id}/revoke /agents/{id}/class) cover permissions only, not agent deletion, and no env or doc names a registry maintenance procedure. Consequence: on any durable bus where common ids collide and keys get lost, the id is a permanent tombstone and the documented happy path fails on first contact. Reproduced live in this fire: 409 on register, 401 on signed DELETE, stale rows from 2026-09-30 still served by GET /agents.' \
  --acceptance-criteria 'Evidence: this fire captured the 409 (register with fresh key for a stale-volume id, alice), the 401 (DELETE /agents/alice signed with the new key), and GET /agents showing 2026-09-30 rows; raw captures under .coding-hermes/evidence/crier-dogfood-2026-11-24-19-47-57/. Fix direction (pick at least one, name it in the AC text): (a) an operator-scoped registry delete (admin-token gate like CR_PERMISSIONS_ADMIN_TOKEN) or an explicit env/CLI maintenance procedure for a lost key, (b) a documented key-rotation transition (old key authorizes replacement of its own identity), (c) a TTL or operator sweep for stale identities. Acceptance: re-registering an id whose original key is lost becomes resolvable by following ONLY the error message and the README registry section (README lines around 872-977); add a regression test that registers alice, refuses a fresh-key registration with 409, then exercises the documented recovery end to end on the durable backend.' \
  --capability-tags 'registry,trust,onboarding' \
  --status pending

printf 'create-515 rc=%s\n' "$?"

boardctl -C $B create \
  --id DF-CRIER-516 \
  --title 'Client onboarding: transcripts abort on re-run (register_if_missing unused) and clients/README omits lease/owner-only semantics a self-built consumer needs' \
  --type task \
  --priority P2 \
  --complexity 2 \
  --reasoning 'Real-use, same dogfood tick. (1) round_trip.py / round-trip.ts / two_agents.py call bare register() and abort with a raw 409 in exactly the re-run case the client itself calls normal: crier_client.register_if_missing() (crier_client.py ~line 979) exists with the docstring a 409 must not read as a broken setup, but none of the three transcripts use it. This fire hit the abort on the first documented run because the shared compose volume already had the ids (DF-CRIER-515 mechanism). (2) clients/README section Using the client in your own code (lines ~44-66) shows a consumer snippet that hides the semantics a self-built consumer trips over, verified by grep: zero hits there for register_if_missing, zero for lease, zero for the owner-only scoping. This fire hit all three from the snippet alone: deliver() to an unregistered target id answers 404 agent not found (delivery targets must be registered agents), stats()/retrieve() of ANOTHER agents inbox answer 403 may only access its own resources, and retrieve() leases everything it returns for lease_seconds (default 30s, README table line ~1078) so an immediate second retrieve returns an empty batch while the lease is live, which reads as lost data.' \
  --acceptance-criteria 'Fix: (a) make the three transcripts idempotent by switching their register call to register_if_missing (they stay transcripts, negative controls unchanged); (b) extend clients/README Using-the-client section with the four facts: re-register is normal via register_if_missing, delivery/refusal 404 for unregistered targets, agent-scoped routes are owner-only (403 from the wrong identity), retrieve leases with lease_seconds default 30s and unacked messages return after expiry (link the README lease table row). Acceptance: python3 clients/python/round_trip.py --server S --id X --key K succeeds TWICE back to back on the same running server without deleting agents (register_if_missing path), and clients/README greps non-zero for register_if_missing, lease_seconds and own-resources; the round-trip negative controls still pass.' \
  --capability-tags 'clients,docs,onboarding' \
  --status pending

printf 'create-516 rc=%s\n' "$?"

echo '=== verify rows in file ==='
jq -c 'select(.id=="DF-CRIER-515" or .id=="DF-CRIER-516") | {id, status, priority, type}' $B/tasks.jsonl
jq -c 'select(.task_id=="DF-CRIER-515" or .task_id=="DF-CRIER-516") | {task_id, event: (.event // .event_type)}' $B/events.jsonl 2>/dev/null | tail -4
