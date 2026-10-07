# Federation Peer Authentication (CR-CHAT-024)

Status: implemented
Date: 2026-10-07
References: specs/CHAT-FEDERATION.md (peer policies, CR-CHAT-023), internal/registry/agentsig.go (per-agent ed25519 signing, CR-FEAT-027), DF-CRIER-6 (shared-secret link auth)

## 1. Problem

Before this change, a federation request that announced a peer identity
(`X-Crier-Fed-Peer`, CR-CHAT-023) announced a CLAIM: the destination's
per-peer policy consulted the named peer's record, but nothing proved the
request actually came from that peer. Any caller that knew a peer id could
borrow its admission grants. Link authentication (DF-CRIER-6) was a single
shared secret — one credential for every peer, no identity, no per-peer
revocation.

## 2. Model

Four pieces, all additive and config-gated:

1. **Peer identity** — each peer has a stable ed25519 keypair. The
   destination holds the peer's PUBLIC key in a peer-auth document
   (`CR_FED_AUTH_FILE`); the peer holds the private key. Same crypto family
   and transcript shape as the per-agent signatures (agentsig.go) — no new
   crypto, one mental model. `crier keygen` (CR-FEAT-027) generates
   compatible PKCS#8 key files.
2. **Mutual auth** — a request that announces a peer identity must carry
   `X-Fed-Ts` (unix seconds) and `X-Fed-Sig` (hex ed25519 signature) over
   the canonical transcript `<METHOD>\n<path>\n<unix-seconds>` — built by
   ONE function (`federation.FedAuthPayload`) on both sides so the wire
   contract cannot drift. Symmetrically, a relay with
   `CR_FED_SELF_KEY_FILE` set signs every outbound forward with its own
   key, so the destination proves the source's identity too. Replay is
   bounded by a ±30 s window (same as agents).
3. **Per-peer authorization** — unchanged from CR-CHAT-023: the policy
   layer (namespaces_allow / agents.allow / agents.deny, default deny)
   decides what a proven peer may reach. The auth layer runs FIRST, so the
   policy consults a PROVEN identity instead of a claim. Refusals keep
   their named bodies (FED_PEER_UNTRUSTED, FED_NAMESPACE_NOT_PERMITTED,
   FED_AGENT_NOT_PERMITTED).
4. **Revocation** — the peer-auth document carries a `revoked` list. A
   revoked peer is refused with `403 FED_PEER_REVOKED` BEFORE signature
   verification (proof success does not admit a revoked identity). The
   document is re-read when its mtime/size changes, so revocation (and
   un-revocation, and new peer registration) takes effect by editing the
   file — no restart, no code change. An invalid document UPDATE keeps the
   last known-good peer set (fail-safe, never fail-open to an empty gate).

## 3. Wire contract

Headers on a peer-announced federation request:

```
X-Crier-Fed-Peer: <peer id>      (the CR-CHAT-023 announcement)
X-Fed-Ts:        <unix seconds>  (replay window anchor)
X-Fed-Sig:       <hex ed25519>   (over "<METHOD>\n<path>\n<ts>")
```

Status codes:

| condition                                   | status | body                      |
|---------------------------------------------|--------|---------------------------|
| no signature headers / bad ts / bad sig     | 401    | named JSON error          |
| peer id with no registered public key       | 401    | named JSON error          |
| forged signature (wrong key)                | 401    | named JSON error          |
| revoked peer (even with valid signature)    | 403    | `FED_PEER_REVOKED`        |
| proven, then policy denies                  | 403    | CR-CHAT-023 FED_* bodies  |

## 4. Configuration

| env var                | meaning                                                              |
|------------------------|----------------------------------------------------------------------|
| `CR_FED_AUTH_FILE`     | path of the peer-identity + revocation document (arms the gate)      |
| `CR_FED_SELF_KEY_FILE` | path of this relay's own PKCS#8 ed25519 private key (outbound signing) |
| `CR_FED_PEERS_FILE`    | per-peer policy document (CR-CHAT-023, unchanged)                    |
| `CR_FED_TOKEN` / `CR_AUTH_TOKEN` | legacy shared-secret Bearer (DF-CRIER-6, unchanged)        |

Peer-auth document shape:

```json
{
  "peers": [
    {"peer": "relay-b", "public_key": "<hex ed25519 public key>"}
  ],
  "revoked": ["relay-b"]
}
```

A bad INITIAL document is a startup failure (an operator's typo must not
look like an empty peer set). No secret material is ever hardcoded: keys
live in operator-managed files.

## 5. Backward compatibility

When `CR_FED_AUTH_FILE` is unset, the middleware is never registered: no
federation request is ever asked for a signature, and the shipped posture —
shared-secret Bearer (DF-CRIER-6), or no auth at all — behaves
byte-for-byte as before. Requests WITHOUT the peer announcement (all local
agent traffic, even on a gated relay) pass through untouched. Outbound
signing is likewise off unless `CR_FED_SELF_KEY_FILE` is set; a destination
without the gate ignores the extra headers. Extend, never replace.

## 6. Non-goals

- No key distribution protocol: the peer-auth document is provisioned by
  the operator (out of band, like the shared secret it replaces).
- No rotation semantics beyond re-editing the document.
- The mesh WebSocket auth (mesh_auth.go) is a separate surface, unchanged.
