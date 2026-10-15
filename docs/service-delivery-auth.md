# Signed service-agent deliveries

This opt-in gate authenticates machine-to-machine POST delivery separately from
human principals in `specs/CHAT-PERMISSIONS.md`. It covers scheduler → router and
router → scheduler, without making Crier a model/provider selector.

## Configuration and identity provisioning

Set this on the Crier server before startup:

```bash
export CR_SERVICE_DELIVERY_ALLOWLIST='lane-scheduler>bunker-router,bunker-router>lane-scheduler'
```

Each entry is one exact, directed `sender>recipient` pair. BOTH directions must
be declared. IDs are case-sensitive and match `[A-Za-z0-9][A-Za-z0-9._:-]*`.
Whitespace around entries/IDs is ignored; internal whitespace, wildcards,
missing halves, extra `>` separators and empty comma entries fail startup.
An empty/unset value disables this gate. Duplicate pairs are harmless.
Handler configuration validates the same grammar and a rejected configuration
leaves its prior policy intact. Configuration is startup-only, not hot-reloaded.

Register each sender's Ed25519 public key via `POST /agents` and keep its private
key with that service. Existing IDs cannot be re-registered with a different
key. Provision IDs/keys over a trusted channel before enabling consumers:
registration itself is NOT authenticated by an agent signature. The allowlist
trusts the key already registered under the exact ID, not a real-world claim
that this process is the scheduler. Protect registration/API access with
`CR_AUTH_TOKEN` and network controls. A shared bearer token alone is NOT proof
of an individual sender's identity; it remains an additional reach gate.

## Signed HTTP contract

For every delivery to ANY recipient appearing on the right side of the list:

- Use `POST /agents/{recipient}/inbox` with that explicit recipient URL.
- Send `X-Agent-ID` equal to the JSON envelope's nonempty `sender`.
- Send `X-Agent-Ts` as Unix seconds, within ±30 seconds of the server clock.
- Send `X-Agent-Body-SHA256` as the lowercase hex SHA-256 of the EXACT raw HTTP
  request body, including whitespace, escaping and trailing bytes.
- Send `X-Agent-Sig` as the hex Ed25519 signature under the sender's registered
  private key of the following FOUR lines, with no trailing newline:

```text
POST
/agents/{recipient}/inbox
<unix-seconds>
sha256:<lowercase-hex-body-digest>
```

The path is Go's `r.URL.Path`, matching the existing agent signature contract;
the query string is excluded. Serialize the body ONCE and sign/send the same
bytes, rather than signing a JSON object and reserializing it differently.
The server recomputes the digest from the bounded raw body and verifies the
actual method/path/timestamp/digest transcript. Missing, empty, malformed,
stale, wrong-key, wrong-path/method and mismatched-digest signatures fail 401;
body/header sender mismatches, human identity fields and unlisted pairs fail
403. An unknown/keyless service sender fails 401, not an implicit allow.

`principal_id` and `as_agent` are human identity/binding fields, NOT a service
credential. Nonempty values are rejected on protected recipients even with a
valid service signature. Once a pair is verified it is the service delivery
authority; the human ACL does not require fabricated principals or bindings for
it. Namespace, containment and content-guard policies still apply downstream.
Ordinary/unconfigured recipients continue using their existing human ACL and
legacy delivery behavior; this opt-in does NOT repair/authenticate those paths.

`reply_to` is only a Crier message/correlation ID. It never changes the recipient
or grants send authority. An unlisted sender cannot gain authority by echoing a
scheduler's ID or a known correlation; altering `reply_to` after signing fails
the digest check. Crier does NOT validate correlation ownership/existence: an
authorized service can assert an arbitrary correlation. Consumers must validate
result-to-dispatch associations as part of their application protocol.

The gate runs before delivery observation, global ingest budget, idempotency
claims/receipts, capability cursor changes, guard calls, persistence and
transports. Protected recipients cannot be reached through capability pools:
a pool containing a protected recipient is refused 403 before rotation/budget
work, and final selection excludes protected agents defensively against a
concurrent capability change. Use explicit recipient URLs for service traffic.
An entirely unprotected pool retains its existing behavior.

## Python client

`clients/python/crier_client.py` automatically body-signs a delivery ONLY when
its explicit `sender` equals the client's own `agent_id` (the identity/key the
caller provisioned or `register()` adopted):

```python
from crier_client import Crier

scheduler = Crier(base_url, agent_id="lane-scheduler", key_path="scheduler.key")
scheduler.register_if_missing()
scheduler.deliver("bunker-router", {"task": "dispatch"}, sender="lane-scheduler")

router = Crier(base_url, agent_id="bunker-router", key_path="router.key")
router.register_if_missing()
router.deliver("lane-scheduler", {"result": "done"}, sender="bunker-router")
```

Missing/another `sender` stays UNSIGNED for metadata-only legacy compatibility;
a protected recipient rejects it. The generic `request(..., signed=True)`
escape hatch refuses a delivery that claims another/missing sender before HTTP.
Signed requests with bodies (including ack) now use the same four-line digest
form. Bodyless GET/delete retain the original three-line form; the server's
self-only retrieve/ack rules are unchanged. Holding the scheduler key NEVER
authorizes retrieving or acknowledging the router's inbox.

The client cannot prove local key/registration correspondence by itself; the
server checks the registered public key on every protected delivery.

## Preservation and limitations

`model`, `provider` and `reply_to` retain the contract in
[router-dispatch.md](router-dispatch.md): concrete/blank strings are not trimmed,
case-folded or filled in; null hints resolve to omission. The authorization gate
neither chooses nor validates model/provider names. The digest protects raw
input bytes, not a promise to preserve JSON formatting on a serialized read.

Freshness is bounded replay exposure, not a single-use nonce. A captured signed
request can be replayed within the 30-second window; use the existing
`idempotency_key` contract when duplicate dispatch matters. Its replay registry
is process-local, not restart-durable. This feature adds no durable replay
ledger, key rotation protocol, federation re-signing or machine attestation.
Intermediaries that rewrite bodies/methods/paths must not be used for signed
service deliveries without preserving this exact contract. TLS is still
required across untrusted networks; signatures do not encrypt content.

Tests exercise real loopback HTTP requests, registered keys, stored inboxes,
replay-state/rate/cursor rejection effects, both directed deliveries and
self-only read/ack. No live deployment is needed to verify this contract.
