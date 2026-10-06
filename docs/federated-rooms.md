# Federated rooms — remote participants, per-peer policy, and the honest crossing statement

CR-CHAT-023 · specs/CHAT-FEDERATION.md §3/§6/§8/§9 · configured with `CR_FED_PEERS_FILE`

Two crier instances can share a room: a member of instance B appears as a
**participant** of a session on instance A, marked REMOTE, and messages fanned
out to that session reach B's member's inbox on B. Each side's members are real
participants whose permissions are decided on the instance they are talking to.

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
| The peer's identity claim (`X-Crier-Fed-Peer`) — a claim, never a credential; the destination's policy still decides | **Peer signatures / keyed auth** (CR-CHAT-024, NOT BUILT): the crossing still rides the shipped `CR_FED_TOKEN` (or unauthenticated links) posture; the peer header authenticates nothing |
| The remote participant's membership event on the local instance | The remote instance's **permission decisions** — they are consulted on the peer's own side only; a role or namespace the peer asserts in a payload is data, never authority (§3.3) |

`crossing: body | reference` (§8.1) is design, not shipped: the shipped forward
carries the whole body, and the reference resolver that would make `reference`
honest does not exist.

## The refusals (all named, never silent)

| Situation | Answer |
|---|---|
| remote join, unknown peer | `403 FED_PEER_UNTRUSTED` |
| remote join / forward, namespace not in `namespaces_allow` | `403 FED_NAMESPACE_NOT_PERMITTED` (outbound wall: the delivery is not forwarded, not held, never a 404) |
| inbound forward, agent denied or not allowed | `403 FED_AGENT_NOT_PERMITTED` |
| inbound forward from an unknown announced peer | `403 FED_PEER_UNTRUSTED` |
| remote target while no federation client is wired | a `refused` outcome on the message record: `FEDERATION_UNCONFIGURED` |

Every refusal names the peer and the decision input in a machine-readable body;
a refusal has no side effect (nothing stored, nothing held, no guard spend).
