# Dogfood integration report — 2026-10-10 — the CR-CHAT wave, used for real

Angle: the 10-01 run covered relay/registry/mesh/guard/MCP. This run used the
NEW surface: chat sessions (CR-CHAT-006/020/028), the session task lifecycle
(D12), named groups (CR-CHAT-013/022), the dual output mode (CR-CHAT-031),
compile, the SQLite backend (CR-CHAT-036), the dagger control surface
(CR-CHAT-033/035) and the keygen→sign→retrieve loop. Server at HEAD 8c316e3,
`CR_SESSION_BACKEND=sqlite`, port 18991.

## What a real consumer does, end to end (all verified live)

1. **Boot with no database service.** `CR_SESSION_BACKEND=sqlite
   CR_SQLITE_PATH=… CR_SESSION_LOG_ROOT=… ./bin/crier` — no PostgreSQL, no
   CGO. Startup log names the enabled session routes. This is the single
   best new feature for a fresh user: a working bus in one command.
2. **Create a session.** `POST /sessions {"title":…,"created_by":{"agent":"…"}}`
   → 201 with an id. Note: `created_by` is an OBJECT with an `agent` member,
   not a string — the 400 error names the Go type (`session.AuthorRef`), which
   is developer-speak; the openapi schema has it, read it first.
3. **Add participants + post.** `POST /sessions/{id}/participants
   {"member_type":"agent","member_id":…,"role":"member"}`; messages take
   `{"payload":{…},"sender":"…","parent_id":…}` (NOT `from` — strict decoding
   rejects unknown fields with a helpful 400). Replies stay in the parent's
   thread automatically; the transcript read is `seq`-ordered.
4. **Tasks.** `POST /sessions/{id}/tasks {"payload":…,"sender":…,
   "targets":[{"kind":"agent","id":…}]}` → 201. ⚠️ The lifecycle beyond
   creation is currently UNUSABLE in real use: no GET on the task, and
   claim/complete 404'd (TASK_NOT_FOUND) on a task created moments earlier —
   DF-CRIER-304.
5. **Named groups.** `POST /groups` (needs `X-Agent-ID` header or created_by)
   with name `dogfood` (the `@team:` prefix is implied — passing
   `team:dogfood` is REFUSED as not addressable). Address it in a message
   body as `@team:dogfood`; the audience becomes a group target. Delivery
   requires every member to be a REGISTERED agent (ed25519 public_key) —
   otherwise outcomes read `refused: agent not found` (DF-CRIER-305).
6. **Registry + inbox round trip.** `./bin/crier keygen -id X -out X.key
   -json -server URL` prints a registration JSON; POST /agents with it.
   Then sign retrieve/ack with ed25519 over "METHOD\npath\nts". Working
   recipe (openssl 3 needs a FILE for oneshot, stdin fails):

   ```bash
   TS=$(date +%s); printf 'GET\n/agents/X/inbox\n%s' "$TS" > m
   openssl pkeyutl -sign -inkey X.key -rawin -in m -out s
   SIG=$(xxd -p s | tr -d '\n')
   curl -H "X-Agent-ID: X" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" $URL/agents/X/inbox
   ```

   Retrieve leases messages; ack needs BOTH `lease_id` AND `message_ids`
   (ack without ids is refused 400 "silent no-op" — good contract). A
   delivery we sent landed in the peer inbox and acked 204; stats went
   queue_depth 1→0. ⚠️ A leased message with no ack within the lease
   window is INVISIBLE on the next retrieve (empty messages array,
   leased_count 1) for ~60s with no wait hint in the response — first-time
   users think the message vanished (we did).
7. **Compile + output modes.** `POST /sessions/{id}/compile` (body shape:
   NOT `from` — see openapi CompileSessionRequest) returns a summary bundle
   naming thread coverage and a trace link; `GET /sessions/{id}/output?mode=trace|summary`
   both 200. The summary correctly covered our 2 threads / 3 messages.
8. **Dagger control (opt-in).** Setting CR_DAGGER_URL registers /dagger/runs;
   the error contract is honest: 502 bridge transport failure with the exact
   upstream error, 404 unknown run. ⚠️ But setting CR_DAGGER_URL WITHOUT
   CR_DAGGER_STORE_DIR prevents the server from booting at all on a
   non-root box (/var/lib/crier mkdir denied) — DF-CRIER-303, the run's P1.
9. **Durability.** Killed the server, restarted on the same sqlite file:
   session, 3 messages, task, group all intact (log-of-record → view
   projection worked).

## Friction census (7 total)

See DF-CRIER-303..306 on the board. The biggest: dagger-configured boot
fails hard on non-root (P1); task lifecycle unusable past creation (P2);
leased-message invisibility on redelivery window (usability, not filed as
defect — behavior is documented in the lease model, but the retrieve
response should say why it is empty).

## Perf

/health 2-16ms, session create 25-110ms, transcript 0.5-18ms, cold start
~1.2s. Nothing user-noticeable → no PERF row beyond the record in
DF-CRIER-308.
