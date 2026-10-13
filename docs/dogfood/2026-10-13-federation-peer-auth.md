# Crier dogfood — federation peer auth (CR-CHAT-024) real-use run, 2026-10-13

Promise under test: "Two relays can prove each other's identity with per-peer
ed25519 keys (CR-CHAT-024) and per-peer policy walls (CR-CHAT-023), and a
member of one bus can sit in a room on the other and receive the fan-out."

Verdict: PROMISING-BUT-ROUGH (behaviour) — the auth gate is real, correct and
honest on every refusal; the federated-room crossing is DEAD on the shipped
binary (DF-CRIER-309).

## What was run (all live, HEAD 7c48745, /tmp/crier-df-fed)

- Fresh build: `go build -o crier ./cmd/server` → `dev-7c487452-dirty`.
- Two relays: alpha :18767 (CR_FED_AUTH_FILE + CR_FED_PEERS_FILE), beta :18768
  (CR_FED_LINKS + CR_FED_SELF_KEY_FILE + CR_FED_PEERS_FILE, self_as=beta).
- Keys: `openssl genpkey -algorithm ed25519`; pub hex from SPKI DER tail.
- Hand-rolled signed forwards (python cryptography lib) against the gate.

### Peer-auth ladder (specs/FEDERATION-AUTH.md §3, all as documented)

| case | result |
|---|---|
| announce without X-Fed-Ts/X-Fed-Sig | 401, error names the missing headers |
| unknown peer id (no key record) | 401 |
| valid ts, signature from a DIFFERENT key | 401 "signature verification failed" |
| signature from the doc key but ts −300s | 401 "outside allowed window (±30s)" |
| valid sig over `POST\n/agents/x/inbox\n<ts>` | passes the gate → normal routing |
| doc touch (mtime change) | picked up live, no restart |
| revoked list + valid sig | 403 FED_PEER_REVOKED, before sig check |
| un-revoke (file edit) | admitted again |
| garbage initial doc + boot | boot ERROR, no silent policy-less relay |
| valid sig, unknown target | 404 "agent not found" (gate passed, routing refused) |

Transcript verified byte-exact: `METHOD\npath\nts`, 3 lines, no trailing
newline; ed25519 over the payload; hex sig. Verified with BOTH python
`cryptography` and `openssl pkeyutl -rawin` — identical signatures.

### CR-CHAT-023 policy walls (alpha inbound, after the auth gate)

- `namespaces_allow: []` (default deny): signed announced forward → 403
  FED_NAMESPACE_NOT_PERMITTED, named body, no side effect.
- `namespaces_allow:[""], agents_deny:["secret-agent"]` → 403
  FED_AGENT_NOT_PERMITTED naming the agent; other agents still 201.
- Policy verified in-process too (AdmitsAgent false for the deny-listed id) —
  the wall logic is right; the live path needed the namespace entry.

### Federated rooms (docs/federated-rooms.md)

- Session create (created_by OBJECT `{\"agent\":\"alice\"}` — a string fails
  INVALID_REQUEST, and the 400 field error names the Go type, finding-level
  friction), remote join `remote:beta:atlas` → 201 `remote:true`,
  participants list reconstructable from storage. All good.
- Fan-out: **refused**. With no CR_FED_LINKS on alpha: honest
  FEDERATION_UNCONFIGURED outcome. WITH links + policy url: WARN
  `peer "beta" is not known to this instance (no policy record)` — because
  cmd/server/main.go never calls fedClient.SetPeerPolicies(pp); the session
  handler's policy comes from the config file, but the fan-out resolves
  policy through the federation Client, whose peerPolicy is never armed in
  production. The unit test (remote_test.go armPeerPolicy) arms the client by
  hand, so the suite is green while the shipped binary cannot cross a room.
  Filed DF-CRIER-309 (P1).

## Performance (Step 2b)

- Signed forward through beta → alpha, hyperfine n=20 warm: 6.4ms ± 0.6
  (curl-fork dominated; server ~0.2-0.9ms per access log). Noisy-host first
  run 10.5ms ± 10.3 noted, re-run clean.
- Cold boot to healthy (peer auth + policies + pg-less jsonl): 75 ms.
- Nothing a user would notice; recorded as DF-CRIER-312, no profile.

## Frictions

1. `-db-url /tmp/x.db` parses as a PostgreSQL DSN → boot fail "failed to
   parse as keyword/value" (contradicts the sqlite story of the 10-10 run;
   used in-memory backends here).
2. `created_by` must be an object, error names the Go struct type, not the
   wire shape.
3. Session fan-out's honest FEDERATION_UNCONFIGURED is right, but nothing
   tells you the destination ALSO needs a non-empty CR_FED_LINKS before the
   policy record's `url` is consulted.

## Artifacts

- Rows: DF-CRIER-309..312 on .coding-hermes/board/tasks.jsonl.
- Probe scripts preserved in /tmp/crier-df-fed/ (boot.sh, ladder.sh,
  policy6.sh, room7.sh, sig.sh, perf.sh).
