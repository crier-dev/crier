# Federated peer auth — the signed-forward recipe

CR-CHAT-024 · specs/FEDERATION-AUTH.md · configured with `CR_FED_AUTH_FILE`,
`CR_FED_SELF_KEY_FILE` and `CR_FED_PEERS_FILE`

A **signed forward** is one relay proving to another that a request really came
from the peer it announces. Before CR-CHAT-024 the announcement header
`X-Crier-Fed-Peer` was a CLAIM — any caller that knew a peer id could borrow that
peer's admission grants. This page is the hands-on recipe: make the keys, configure
both relays, sign the exact transcript crier verifies, send the forward, and verify
a signature offline. Every command and every refusal body below was executed
against a live server (OpenSSL 3.5.5, crier `2f9f302`; probes recorded in the
DF-CRIER-311 tick).

Everything is **config-gated and additive**. With `CR_FED_AUTH_FILE` unset no
request is ever asked for a signature, and the shipped shared-secret posture
(`CR_FED_TOKEN` / `CR_AUTH_TOKEN`) behaves byte-for-byte as before.

## 1. Who holds what

| side | env var | file / record | holds |
|---|---|---|---|
| source (relay-a) | `CR_FED_SELF_KEY_FILE` | PKCS#8 PEM ed25519 **private** key | signs every outbound forward |
| source (relay-a) | `CR_FED_PEERS_FILE` | policy record for the peer, `self_as` | the identity relay-a announces (`X-Crier-Fed-Peer`) and the link URL |
| destination (relay-b) | `CR_FED_AUTH_FILE` | `peers[]` + `revoked[]` | the **public** key it accepts for each peer id — this is what arms the gate |
| destination (relay-b) | `CR_FED_PEERS_FILE` | policy record for the announced peer | which namespaces and agents that peer may reach (CR-CHAT-023, default deny) |

Mutual auth is the point: both sides hold a key and each proves itself to the
other. The auth layer runs **before** the policy layer, so the policy consults a
PROVEN identity rather than a string the caller chose. A request with no
`X-Crier-Fed-Peer` header at all (every local agent call) never meets this gate.

## 2. The wire contract

Headers on a peer-announced federation request:

| header | value |
|---|---|
| `X-Crier-Fed-Peer` | the announced peer id (the key the destination's policy resolves) |
| `X-Fed-Ts` | unix seconds, the replay-window anchor |
| `X-Fed-Sig` | hex ed25519 signature over the transcript below |
| `X-Crier-Fed-Hop` | `1` — marks the request as already forwarded (loop prevention; also what makes the ACL treat the caller as a remote principal) |

The transcript is **exactly** the bytes `federation.FedAuthPayload` builds
(`internal/federation/peerauth.go`):

```
<METHOD>\n<path>\n<unix-seconds>
```

Four properties of that string are load-bearing, and each was measured:

1. **`<METHOD>` is the uppercase method** — `POST` for a forward.
2. **`<path>` is the DECODED URL path**, query string excluded (Go's
   `r.URL.Path`). Signing the percent-encoded form fails: a request to
   `/agents/agent%2Dx/inbox` verifies only against `/agents/agent-x/inbox`, and
   `?trace=1` is not part of the transcript.
3. **`<unix-seconds>` is the byte-for-byte string you put in `X-Fed-Ts`**, not a
   re-formatted integer. A header of `01791905457` verifies against a transcript
   containing `01791905457`.
4. **The window is ±30 s** (`fedAuthSkew`, the same bounded-replay window the
   per-agent signatures use).

Hex is case-insensitive; the signature must decode to exactly 64 bytes.

## 3. Step 1 — make the keypair (source relay)

`CR_FED_SELF_KEY_FILE` is a **PKCS#8 PEM ed25519 private key** — the same shape the
per-agent signing workflow and `crier keygen` already write, so one key format
covers the whole system:

```bash
openssl genpkey -algorithm ED25519 -out relay-a.key
# -> -----BEGIN PRIVATE KEY-----   (PKCS#8)
```

Extract the hex public key the destination will store (the SPKI DER envelope is
12 bytes, so the last 32 bytes are the raw key):

```bash
RELAY_A_PUB=$(openssl pkey -in relay-a.key -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
echo "$RELAY_A_PUB"      # 64 hex chars = the ed25519 public key
```

No openssl and no xxd available? `crier keygen` writes the same file and prints
the same hex:

```bash
./bin/crier keygen -out relay-a.key -id relay-a -json     # -> "public_key": "<64 hex>"
```

## 4. Step 2 — configure the destination (relay-b)

Two documents. The **auth document** (`CR_FED_AUTH_FILE`) is what arms the gate:

```json
{
  "peers": [
    {"peer": "relay-a", "public_key": "<RELAY_A_PUB, 64 hex chars>"}
  ],
  "revoked": []
}
```

The **policy document** (`CR_FED_PEERS_FILE`, CR-CHAT-023) decides what the proven
peer may then reach. `namespaces_allow` is canonical: the default realm is spelled
`""`, so a peer delivering into the default namespace needs `""` in that list:

```json
{
  "peers": [
    {"peer": "relay-a", "namespaces_allow": [""], "agents_deny": []}
  ]
}
```

Write both files from the shell, so no 64-character hex is ever hand-edited:

```bash
printf '{"peers":[{"peer":"relay-a","public_key":"%s"}],"revoked":[]}\n' "$RELAY_A_PUB" > auth.json
printf '{"peers":[{"peer":"relay-a","namespaces_allow":[""],"agents_deny":[]}]}\n' > peers.json
```

Start the destination with both:

```bash
CRIER_PORT=8767 CR_FED_AUTH_FILE=./auth.json CR_FED_PEERS_FILE=./peers.json ./bin/crier
```

Two failure modes worth knowing before they surprise you:

- a **bad INITIAL** auth document is a **startup failure** (the server logs
  `load federation peer auth file` and exits 1) — an operator's typo must not look
  like an empty peer set;
- an **update** to the auth document keeps the last known-good state if the new
  bytes are invalid (fail-safe, never fail-open to an empty gate).

## 5. Step 3 — configure the source (relay-a)

```bash
CRIER_PORT=8767 \
CR_FED_LINKS="http://relay-b:8767" \
CR_FED_SELF_KEY_FILE=./relay-a.key \
CR_FED_PEERS_FILE=./peers-a.json \
./bin/crier
```

with `peers-a.json` naming the link and the identity to announce:

```json
{
  "peers": [
    {
      "peer": "relay-b",
      "url": "http://relay-b:8767",
      "self_as": "relay-a",
      "namespaces_allow": [""]
    }
  ]
}
```

`self_as` is the string that becomes `X-Crier-Fed-Peer`, so it must be the peer id
**relay-b's auth document registers for relay-a**. Note that
`CR_FED_SELF_KEY_FILE` is only consulted when `CR_FED_LINKS` is non-empty: no
links, no signing, and a broken key file goes unnoticed (measured — see §10).

Once running, every outbound forward carries `X-Fed-Ts` / `X-Fed-Sig`
automatically (`SignFedRequest`); the log line `federation outbound signing` means
the key loaded.

## 6. Step 4 — sign the transcript by hand

This is the whole ceremony, and it is what a hand-rolled client (or a `curl`
firefight at 03:00) needs:

```bash
PATH_ON_WIRE="/agents/agent-x/inbox"
METHOD="POST"
TS=$(date +%s)

printf '%s\n%s\n%s' "$METHOD" "$PATH_ON_WIRE" "$TS" > /tmp/fed-transcript.bin
SIG=$(openssl pkeyutl -sign -rawin -inkey relay-a.key -in /tmp/fed-transcript.bin | xxd -p -c 128)

wc -c < /tmp/fed-transcript.bin   # 37 bytes for the example above
echo "${#SIG}"                    # 128 — 64 bytes as hex
```

Measured: `openssl pkeyutl -sign -rawin` returned **rc 0**, producing 128 hex
characters for a 37-byte transcript, and the destination answered **201** with
these exact headers — the recipe is not a sketch, it is a signed forward a real
crier accepted.

Two `pkeyutl` traps, both fatal in silence:

- `-rawin` is **OpenSSL 3+**. Check with
  `openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'` before blaming the server.
- `-sign -rawin` is a **one-shot** operation and needs a seekable payload: pass it
  with `-in <file>`. A piped or process-substituted payload fails with
  `unable to determine file size for oneshot operation` and yields a **zero-byte**
  signature, which the destination rejects as malformed. Never pipe the transcript.

## 7. Step 5 — send the forward

```bash
curl -s -X POST "http://relay-b:8767${PATH_ON_WIRE}" \
  -H 'Content-Type: application/json' \
  -H 'X-Crier-Fed-Hop: 1' \
  -H 'X-Crier-Fed-Peer: relay-a' \
  -H "X-Fed-Ts: ${TS}" \
  -H "X-Fed-Sig: ${SIG}" \
  --data-binary '{"payload":{"hello":"world"}}'
# 201 {"id":"…","transport":"inbox","expires_at":"…"}

# If the destination sets CR_AUTH_TOKEN, the shared secret is checked FIRST:
# add -H "Authorization: Bearer ${CR_FED_TOKEN}" or the answer is
# 401 {"error":"missing Authorization header"} — the auth middleware runs
# outermost, before the peer gate, so a valid signature alone is not enough.
```

`X-Crier-Fed-Hop: 1` is not decoration. It is what stops a relay that does not
know the target agent from re-forwarding the request over its own links (loop
prevention), and it is also what makes the ACL treat the caller as a **remote**
principal instead of an anonymous one.

## 8. Step 6 — verify a signature offline

With the signer's private key (the sanity check):

```bash
openssl pkey -in relay-a.key -pubout -out relay-a.pub.pem
printf '%s' "$SIG" | xxd -r -p > /tmp/fed-sig.bin
openssl pkeyutl -verify -rawin -pubin -inkey relay-a.pub.pem \
  -in /tmp/fed-transcript.bin -sigfile /tmp/fed-sig.bin
# Signature Verified Successfully   (rc 0)
```

With only the **hex public key** the destination already has — reconstruct the
SPKI envelope (`302a300506032b6570032100` is the fixed 12-byte ed25519 prefix)
instead of asking anyone for a secret:

```bash
printf '302a300506032b6570032100' | xxd -r -p > /tmp/relay-a.spki.bin
printf '%s' "$RELAY_A_PUB"        | xxd -r -p >> /tmp/relay-a.spki.bin
openssl pkey -pubin -inform DER -in /tmp/relay-a.spki.bin -out relay-a.pub.pem
openssl pkeyutl -verify -rawin -pubin -inkey relay-a.pub.pem \
  -in /tmp/fed-transcript.bin -sigfile /tmp/fed-sig.bin
# Signature Verified Successfully   (rc 0)
```

Measured: both paths verify, and the PEM rebuilt from the hex key is
**byte-identical** to the one derived from the private key. That is how an operator
confirms "the signature proves the identity in my auth document" without ever
touching the private key.

## 9. The named refusals (verbatim, measured)

Status is the contract; the body names the reason. All bodies are `application/json`.

| condition | status | body |
|---|---|---|
| announced peer, no signature headers | 401 | `{"error":"federation peer \"relay-a\" announced an identity but is missing signature headers (X-Fed-Ts, X-Fed-Sig)"}` |
| `X-Fed-Ts` not a number | 401 | `{"error":"X-Fed-Ts must be a unix timestamp in seconds"}` |
| timestamp outside the ±30 s window | 401 | `{"error":"X-Fed-Ts outside allowed window (±30s)"}` |
| `X-Fed-Sig` not hex / not 64 bytes | 401 | `{"error":"X-Fed-Sig is malformed: expected the hex encoding of a 64-byte ed25519 signature"}` |
| signature does not verify (forged key, wrong transcript) | 401 | `{"error":"federation peer \"relay-a\" signature verification failed"}` |
| peer id with no registered public key | 401 | `{"error":"federation peer \"relay-ghost\" has no registered identity (no public key record)"}` |
| **revoked** peer — even with a valid signature | 403 | `{"error":"federation peer \"relay-a\" is revoked (FED_PEER_REVOKED): the operator has disabled this identity"}` |
| proven, but the policy does not admit the namespace | 403 | `{"error":"FED_NAMESPACE_NOT_PERMITTED","peer":"relay-a","direction":"inbound","target":{"type":"agent","ref":"agent-x"},"detail":"… (namespaces_allow)"}` |
| proven, but the peer has no policy record | 403 | `{"error":"FED_PEER_UNTRUSTED","peer":"relay-a","direction":"inbound","target":{"type":"agent","ref":"agent-x"},"detail":"peer \"relay-a\" is not known to this instance (no policy record)"}` |
| proven, but the agent is denied | 403 | `{"error":"FED_AGENT_NOT_PERMITTED","peer":"relay-a","direction":"inbound","target":{"type":"agent","ref":"agent-x"},"detail":"… (agents.allow/agents.deny)"}` |
| `CR_AUTH_TOKEN` set and no Bearer sent (checked FIRST) | 401 | `{"error":"missing Authorization header"}` |

Note the two body shapes: the **peer-auth** gate (this feature) answers a flat
`{"error":"…"}` whose message carries the code (`FED_PEER_REVOKED`), while the
**policy** gate (CR-CHAT-023) answers the structured body above. A client that
switches on a field should read `error` as a string, never as an object.

A 403 on a valid signature is intentional: **revocation is checked BEFORE the
signature**, so a revoked peer gets the same answer whether or not it still holds
its key — proof success does not admit a revoked identity.

## 10. Behaviour that will bite you (each measured)

1. **Bearer runs before the peer gate.** On a destination with `CR_AUTH_TOKEN`
   set, a forward with a perfect signature and no `Authorization` header is
   answered `401 {"error":"missing Authorization header"}`. Both credentials, or
   neither gate passes.
2. **The policy document is read at STARTUP; the auth document hot-reloads.**
   Revoking a peer is a file edit (mtime/size change, no restart — measured:
   `revoked:["relay-a"]` → 403, remove it → 201 again). Changing
   `CR_FED_PEERS_FILE` needs a restart; edits to a running process were ignored.
3. **`CR_FED_SELF_KEY_FILE` needs `CR_FED_LINKS`.** With no links configured the
   server never loads the self key — measured: a deliberately invalid key file on a
   link-less server started normally and served `/health` (the run had to be
   killed by a timeout). A broken key file is a startup failure only on a relay
   that actually forwards.
4. **Sign the exact bytes.** Re-formatting the timestamp (`date -u`, a
   `%d`-formatted integer) or signing the encoded path changes the transcript and
   fails with `signature verification failed` — not with a helpful "transcript
   mismatch".
5. **An unannounced request is never asked for a signature.** Local agent traffic
   on a gated relay is untouched (measured: 201 with no peer header). The gate keys
   off `X-Crier-Fed-Peer`, so dropping that header is not a bypass — it is
   "no peer identity declared", which routes the request down the shipped path.
6. **Keys are files, never secrets in config.** Nothing in this feature is
   hardcoded; the private key lives in an operator-managed `0600` file, and the
   destination only ever needs public keys.

## 11. Spec vs code, where they drift

The shipped code is the contract; these are the places a reader will otherwise be
misled (all three are reported on the DF-CRIER-311 tick):

- `config/config.go` describes `CR_FED_SELF_KEY_FILE` as a **"hex ed25519 private
  key (seed) file"**. It is not: the loader
  (`registry.LoadEd25519PrivateKeyFile`) demands a **PKCS#8 PEM** and a hex seed
  file is refused at startup with `no PEM data found in private key file`
  (measured, exit 1). `specs/FEDERATION-AUTH.md` §4 says PKCS#8 and is right.
- `cmd/server/main.go` says a missing/invalid self key file *"is a startup failure,
  not a silent unsigned mode"* — true only when `CR_FED_LINKS` is non-empty (§10.3).
- `specs/CHAT-FEDERATION.md` §5 still presents the peer auth as **NOT BUILT**, with
  proposed names (`CR_FED_INSTANCE_KEY`) and proposed codes
  (`FED_PEER_UNAUTHENTICATED`, `FED_PEER_SIGNATURE_INVALID`,
  `FED_PEER_KEY_REQUIRED`) that CR-CHAT-024 did not ship. The shipped contract is
  `specs/FEDERATION-AUTH.md` and this page. That section is a stale proposal, not a
  description of the running system.

## See also

- [`specs/FEDERATION-AUTH.md`](../specs/FEDERATION-AUTH.md) — the normative spec for
  this feature (model, wire contract, configuration, backward compatibility).
- [`docs/federated-rooms.md`](federated-rooms.md) — remote participants and the
  per-peer policy layer (`CR_FED_PEERS_FILE`, CR-CHAT-023) this gate protects.
- [`specs/CHAT-FEDERATION.md`](../specs/CHAT-FEDERATION.md) §4/§6 — the peer model
  and policy record the destination resolves after the identity is proven.
- [`docs/integration-guide.md`](integration-guide.md) — the wider delivered →
  retrieve → ack round trip these forwards ride on.
