# Detection & containment (CR-FEAT-030)

Status: **implemented** (opt-in). Normative for the delivery log format, the
alert signals, the kill-switch's action set and the canary contract.

## 1. The gap this closes

Crier can attribute. Every agent holds an ed25519 key, every delivery passes one
choke point, and a delivery that fails leaves a durable receipt in the sender's
inbox. Ask it *who* sent something and it answers precisely — **afterwards**.

Ask it whether it could have seen a compromise *while it was happening* and the
honest answer before this change was no: there was no record of the delivery
stream, no notion of what an agent's traffic normally looks like, and no way to
stop an agent short of an operator deleting it by hand, one endpoint at a time.

This layer is wiring, not a new trust primitive. It consumes the three pieces
the bus already had — per-agent identity, the choke point, the receipts — and
adds four things an operator running a network with adversaries on it needs:
a log that cannot be rewritten, signals that fire on behaviour, one call that
contains an agent, and a trap for exfiltration.

## 2. Opt-in, and what "off" means

`CR_DETECT_ENABLED` (default `false`) is the master switch. With it unset:

- none of the five routes in §6 is registered (each answers `404` like any
  unregistered path);
- no log file is written and no key is generated;
- the delivery path is **byte-identical** to a build without this feature: the
  detector is nil, every hook is a nil check, and no response body, status code
  or header changes.

Nothing here is auth-exempt. The kill-switch is the most powerful call on the
bus; it demands the operator's Bearer token like every other route.

## 3. The delivery log

### 3.1 What is recorded

One record per delivery outcome, plus one per alert and one per containment
call. A delivery record carries exactly the question an incident review asks:

| field | meaning |
|-------|---------|
| `seq` | 1-based position in this file, contiguous |
| `at` | RFC 3339 UTC timestamp |
| `kind` | `delivery` \| `alert` \| `containment` |
| `sender` | the identity the request claimed (as crier saw it) |
| `target` | the agent the delivery was addressed to |
| `message_id` | the message id the bus minted, when one was minted |
| `verdict` | what the bus decided (see §3.2) |
| `transport` | `inbox` \| `webhook` \| `federation`, when it applies |
| `signal`, `detail` | the alert signal, or the containment summary |

### 3.2 Verdicts

`delivered` (stored in the target's inbox), `webhook_accepted` (handed to the
target's webhook transport), `webhook_failed`, `federation_held`,
`federation_failed`, `guard_blocked`, `quarantined`, `agent_not_found`,
`rate_limited` (the global ingest budget shed the delivery — 429
`RATE_LIMITED_GLOBAL`, CR-FEAT-035: the request was fine and the bus was full,
which is not the same finding as `rejected`), `rejected`, `unspecified`.

The verdict is measured from **what the caller received** — the handler wraps
its own response and maps the status it wrote — not re-derived from which branch
looked like it ran. A branch that returns early is still logged. That is the
whole point of a log: it is not selective.

### 3.3 Signed, chained, and honest about tampering

Each record's `hash` is the sha256 of the record with `hash` and `signature`
blanked; the signature is the server's ed25519 signature over that hash; and the
record's `prev_hash` names its predecessor, whose value is inside the hash. The
consequences, all of them tested:

- **editing** a record breaks its hash and its signature;
- **deleting or reordering** a record breaks the sequence and the chain from
  that point on;
- **appending** a record signed by another key fails signature verification.

The signing key is a FILE (`CR_DETECT_KEY`, default `<CR_DETECT_LOG>.key`,
created `0600`). It is never regenerated silently: an unreadable, non-hex or
wrong-length key file is a startup error, because a per-boot key would make
every restart an unverifiable log.

**A log that does not verify is refused at startup** — the server exits rather
than appending to a history that no longer means anything. A log that is
tampered with *while the server runs* is reported by `GET /delivery-log/verify`,
which re-reads the file (never the in-memory window) and names the first record
that fails.

### 3.4 Durability, and its cost

Every record is appended `O_APPEND` and fsynced before `Append` returns, so a
record is on disk when the delivery response is written. That is one fsync per
delivery: measurable on a high-throughput relay. It is opt-in for exactly that
reason, and the read API bounds a page to 1000 records while reporting the true
file total separately, so a window is never mistaken for the whole log.

## 4. Signals

All four are **advisory**: nothing is contained automatically. Containment is an
operator decision, and the recommendation is explicit — the alert tells you what
tripped, the kill-switch (§5) is one call away.

One alert per (signal, agent) per window: a sustained spike is one alert, not
one per message.

| signal | severity | trips when | default | evidence |
|--------|----------|------------|---------|----------|
| `fanout_spike` | critical | one sender reaches N **distinct** targets in the window | 5 in 60s | `distinct_targets`, `targets`, `window_seconds`, `threshold` |
| `new_peer_burst` | critical | one sender opens N first-ever conversations in the window | 3 in 60s | `new_peers`, `targets`, `window_seconds`, `threshold` |
| `odd_hour_volume` | warning | one sender delivers N messages inside the quiet window | 3 in 01:00-05:00 UTC | `quiet_hours_utc`, `messages`, `threshold` |
| `canary_trip` | critical | a delivery is addressed to a canary id, or carries a canary token | 1 | `canary_id`, `target`, `message_id`, `verdict` |

Baselines are per **sender**, and they observe accepted outreach: the bus's own
refusals are not behaviour. Specifically, a delivery refused *because the agent
is contained* is recorded but never counted toward a baseline — otherwise a
contained agent would look like a permanent spike, which is the opposite of a
useful signal.

Deliveries with no claimed sender are recorded but not baselined: there is no
per-agent baseline to update.

Window and threshold are per signal (`CR_DETECT_FANOUT_WINDOW_S`,
`CR_DETECT_FANOUT_MIN_TARGETS`, `CR_DETECT_NEWPEER_WINDOW_S`,
`CR_DETECT_NEWPEER_MIN_TARGETS`, `CR_DETECT_QUIET_HOURS`,
`CR_DETECT_QUIET_MIN_MESSAGES`). A declared value that cannot be honored is a
startup error naming the variable, never a silently ignored setting.
`CR_DETECT_QUIET_HOURS=0-0` disables the odd-hour signal.

## 5. The kill-switch

`POST /agents/{id}/kill-switch`, body `{"reason":"..."}` (optional, ≤512 bytes,
recorded in the log). ONE call performs four actions, in this order — and the
order is the contract:

1. **`pause_webhooks`** — the agent's outbound webhook lane stops and what it
   had queued is dropped (dead-lettered the way v1 dead-letters: one structured
   log line, counted in `webhook_deliveries_total{outcome="dropped"}`). It runs
   FIRST so nothing keeps leaving after the call.
2. **`revoke_leases`** — the unacked messages the agent had claimed go back to
   the queue; an ACKed message is never resurrected, and no message is deleted.
   The count is the forensic answer to "what did it have in hand?".
3. **`quarantine`** — from here the delivery path refuses both directions with
   `403 AGENT_QUARANTINED` (`"side":"sender"` or `"side":"target"`), before the
   message id is minted, before federation and before the guard.
4. **`unregister`** — the registry row is removed (idempotent: an absent row is
   reported as such, not as a failure).

Each action is reported with its own status — `ok`, `unsupported`, `error` —
and `contained` is true only when the actions that make the agent harmless all
succeeded. `unsupported` is a real third state: a store that cannot revoke
leases (the remote store proxies reads and writes to another server, and has no
endpoint for this) says so, and the response carries a warning. A partial
containment can never read as a clean one.

What the kill-switch does **not** do, stated so an operator does not over-trust
it: a webhook POST already in flight is not recalled; the outbound pause is
per-process (the quarantine and the row removal are the states that survive);
and containment is not automatic — nothing in this layer contains an agent on
its own.

## 6. Surfaces

| route | answers |
|-------|---------|
| `GET /delivery-log` | `limit` (default 100, max 1000) and `kind` (`delivery`\|`alert`\|`containment`) are honored or refused (400); the body carries the page, the file total, the path, the key id and a full-file verification report |
| `GET /delivery-log/verify` | a fresh read of the file: `ok`, `entries`, `first_bad_seq`, `error`, `key_id` — `409` when the log does not verify |
| `GET /alerts` | every alert raised, oldest first, with `?signal=` filtering and the thresholds in force |
| `GET /canaries` | the planted canaries (id + token) and the count |
| `POST /agents/{id}/kill-switch` | the containment report of §5 |

## 7. Canary tokens

A canary is a string planted where an exfiltrating agent would find it (a file
on the host, an env var, an agent's memory) whose appearance anywhere it does
not belong is itself the finding. Crier plants them and watches the one channel
it owns — deliveries:

- a delivery **addressed to** a canary id (the id is derived from the token, so
  it is stable across restarts and can be denylisted); or
- a delivery whose **payload contains** a canary token

trips `canary_trip` (critical). Payloads above 64KiB are not scanned for tokens
(the address check still applies): scanning megabytes on the delivery path would
be a self-inflicted denial of service.

`CR_CANARY_TOKENS` pins the tokens; unset, the server generates
`DefaultCanaryCount` (2) per boot, logs them at startup and serves them at
`GET /canaries`. Generated canaries rotate per boot — pin the tokens to plant
them somewhere durable.

## 8. What this does not claim

- **It is not automatic containment.** Signals are advisory.
- **It is not a tamper-proof audit trail against root.** The log is signed and
  chained, so a rewrite is DETECTED; the records live in one file on one host,
  and an attacker with the signing key (or the ability to replace the whole file
  and the key, and restart the server) is out of scope. Ship the file off-host if
  that is your threat model.
- **It is not a baseliner that learns.** The three signals are fixed, documented
  counters with explicit windows and thresholds. Nothing is inferred, and a
  threshold an operator has not tuned will either miss or nag — that is a
  property of counting, not a hidden model.
- **It does not read the mesh or the relay.** A compromised agent that only ever
  publishes to a relay topic is not covered by these baselines; deliveries are
  the observed channel.
- **The log window served by the API is a window** (the last 10000 records in
  memory). The file is the record of record.

## 9. Acceptance

The acceptance criterion is a scripted "compromised agent" that fans out to N
new peers, trips a documented alert, and is contained by the kill-switch in one
call — captured live. That run is
`TestDetectionCatchesAndContainsACompromisedAgent` (`cmd/server/crfeat030_test.go`),
executed against the real server wiring (`run(nil)`), and it additionally proves
that the log survives a restart with its chain intact and continues at the next
sequence number.

Source of this row: the external hands-on review "DISPATCH · CRI-001" by Carter
(2026-09-25), which named detection — not attribution — as the gap that would
have mattered in the July-2026 agent-intrusion incident.
