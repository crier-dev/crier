# Federated rooms — remote participants, per-peer policy, and the honest crossing statement

CR-CHAT-023 · specs/CHAT-FEDERATION.md §3/§6/§8/§9 · configured with `CR_FED_PEERS_FILE`

Two crier instances can share a room: a member of instance B appears as a
**participant** of a session on instance A, marked REMOTE, and messages fanned
out to that session reach B's member's inbox on B. Each side's members are real
participants whose permissions are decided on the instance they are talking to.

**Peer identity is provable, not just claimed (CR-CHAT-024).** A federation
forward that announces a peer identity can be signed by that peer's ed25519 key
over `<METHOD>\n<path>\n<unix-seconds>` (`X-Fed-Ts` / `X-Fed-Sig`), verified against
the destination's `CR_FED_AUTH_FILE` before this policy layer consults the
announcement — so the policy below decides what an identity PROVEN to be the peer
may reach. Make the keys, sign a forward and read the refusal bodies:
[`docs/federated-peer-auth.md`](federated-peer-auth.md).

## The remote participant

A member of a peer joins a local session with a **remote-qualified id**:

```
remote:<peer id>:<external subject id>
```

Example: `remote:peer_acme:atlas` — the subject `atlas` as it is known on the
peer A calls `peer_acme`. The join is admitted only when:

1. the peer has a **policy record** on this instance (otherwise
   `403 FED_PEER_UNTRUSTED`), and
2. that policy admits the session's namespace (`403 FED_NAMESPACE_NOT_PERMITTED`
   otherwise — the default is deny).

The participant list (`GET /sessions/{id}/participants`) then shows the member
with `"remote": true`. The marker is derived from the recorded id, so it is
reconstructable from storage alone; the transcript names the remote participant
in its remote-qualified identity, never as a local one.

## The per-peer policy (CR_FED_PEERS_FILE)

```json
{
  "peers": [
    {
      "peer": "peer_acme",
      "url": "https://crier.acme.example",
      "self_as": "peer_beta",
      "namespaces_allow": ["acme-mirror", ""],
      "agents_allow": ["atlas"],
      "agents_deny": ["deploy-bot"]
    }
  ]
}
```

| Field | Rule |
|---|---|
| `peer` | the LOCAL name for the peer — the handle refusals and remote ids carry. |
| `url` | the peer's base URL, used by the shipped forward. Absent = inbound-only: the peer's members may join, but nothing crosses TO the peer. |
| `self_as` | the identity THIS instance announces to that peer on a forward (`X-Crier-Fed-Peer` header), keyed by the destination's own policy set. Absent = no announcement. |
| `namespaces_allow` | which namespaces may cross the boundary, in either direction. **Empty means none — default deny.** `""` names the default realm. |
| `agents_allow` / `agents_deny` | which local agents an inbound forward from this peer may reach. `deny` wins over `allow`. |

The policy is a **wall, not a grant** (§6.2): it decides who may cross; what a
crossing participant may then do is decided by the local model, unchanged.

Set `CR_FED_PEERS_FILE=/path/to/peers.json`. A file that cannot be read or
parsed is a **boot error**, never a silently policy-less relay. With the
variable unset, no peer exists and the instance federates exactly as before
(`CR_FED_LINKS` / `CR_FED_TOKEN` untouched).

## What actually crosses — the honest statement (§8)

| Crosses | Does NOT cross (yet) |
|---|---|
| The message **body** (the deliver payload), as the shipped forward has always carried it | **Assets** (CR-CHAT-014, NOT BUILT): an S3 asset reference crosses as a pointer only; asset BYTES cross only when inline in the payload, and no credential to dereference an asset is ever handed across |
| The addressing context: session id, thread id, message id, sender agent id, the session's namespace, the message kind, the deterministic session idempotency key | **Audit surfaces** (CR-CHAT-021, NOT BUILT): the local instance records the crossing in its own delivery log, but there is no shared or replicated audit trail between the two instances |
| The peer's identity claim (`X-Crier-Fed-Peer`) — a claim by default, and **provable since CR-CHAT-024**: where the peer-auth gate is armed (`CR_FED_AUTH_FILE`) the announcement must carry `X-Fed-Ts` + `X-Fed-Sig` — a hex ed25519 signature over `<METHOD>\n<path>\n<unix-seconds>` (±30 s window) verified against the announced peer's registered public key BEFORE the per-peer policy runs, so the policy consults a proven identity instead of a string the caller chose; this instance signs its own outbound forwards the same way when `CR_FED_SELF_KEY_FILE` is set (`specs/FEDERATION-AUTH.md`; hands-on recipe: [`docs/federated-peer-auth.md`](federated-peer-auth.md)) | **Proof on a deployment that has not armed the gate** (CR-CHAT-024 — shipped, but OPT-IN): with `CR_FED_AUTH_FILE` unset the middleware is never registered, so no request is ever asked for a signature and the announced `X-Crier-Fed-Peer` remains a claim — the crossing rides the shipped `CR_FED_TOKEN` / `CR_AUTH_TOKEN` (or unauthenticated links) posture byte-for-byte as before. There is also no key-distribution or link-handshake protocol: the peer document (`peers` + `revoked`) is provisioned out of band by the operator (§6 non-goals) |
| The remote participant's membership event on the local instance | The remote instance's **permission decisions** — they are consulted on the peer's own side only; a role or namespace the peer asserts in a payload is data, never authority (§3.3) |

`crossing: body | reference` (§8.1) is design, not shipped: the shipped forward
carries the whole body, and the reference resolver that would make `reference`
honest does not exist.

Peer auth is the one crossing credential that is now verifiable rather than
asserted — `specs/FEDERATION-AUTH.md` (CR-CHAT-024, implemented) is its sibling
spec; §3 there is the wire contract (headers + statuses) and the code is
`internal/federation/peerauth.go`. Two limits are stated, not implied: it is
armed per deployment (`CR_FED_AUTH_FILE` on the receiving side,
`CR_FED_SELF_KEY_FILE` to sign outbound), and it authenticates a FORWARD — which
peer sent this request — never a direction or a link state; CR-CHAT-023's policy
record still decides what a proven peer may reach, and a peer on the document's
`revoked` list is refused `403 FED_PEER_REVOKED`, before its signature is even
checked. The document is re-read when it changes on disk, so revoking a peer (or
registering one) is a file edit, not a restart. An unproven announcement is
refused with a named 401 body (`X-Fed-Ts`/`X-Fed-Sig` missing, a timestamp
outside the ±30 s window, a peer id with no registered public key, or a
signature that does not verify).

## The refusals (all named, never silent)

| Situation | Answer |
|---|---|
| remote join, unknown peer | `403 FED_PEER_UNTRUSTED` |
| remote join / forward, namespace not in `namespaces_allow` | `403 FED_NAMESPACE_NOT_PERMITTED` (outbound wall: the delivery is not forwarded, not held, never a 404) |
| inbound forward, agent denied or not allowed | `403 FED_AGENT_NOT_PERMITTED` |
| inbound forward from an unknown announced peer | `403 FED_PEER_UNTRUSTED` |
| announced peer with no valid signature (gate armed by `CR_FED_AUTH_FILE`) | `401` + a named body: `X-Fed-Ts`/`X-Fed-Sig` missing, a timestamp outside the ±30 s window, a peer id with no registered public key, or a signature that does not verify |
| announced peer on the document's `revoked` list | `403 FED_PEER_REVOKED` — checked BEFORE the signature, so proving the identity never readmits a revoked one |
| remote target while no federation client is wired | a `refused` outcome on the message record: `FEDERATION_UNCONFIGURED` |

Every refusal names the peer and the decision input in a machine-readable body;
a refusal has no side effect (nothing stored, nothing held, no guard spend).
