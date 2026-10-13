# Dogfood diagnostics — 2026-10-13, federation peer auth (CR-CHAT-024)

## How the surface is built

- `internal/federation/peerauth.go` — PeerAuth: loads CR_FED_AUTH_FILE
  (peers[] + revoked[]), re-reads on mtime/size change, middleware refuses any
  request announcing X-Crier-Fed-Peer without valid X-Fed-Ts/X-Fed-Sig over
  `FedAuthPayload(method, path, ts)` = `METHOD\npath\nts` (one function on
  both sides so the wire cannot drift). Bad initial doc = boot failure.
- `internal/federation/federation.go` — Client.ForwardDeliver: announces
  self_as from the LINK's policy record, signs every forward when SetSelfKey
  armed; SetPeerPolicies arms Client.peerPolicy (for PolicyFor).
- `internal/registry/handler.go` deliver path — inbound wall: unknown peer /
  namespace / agent refusals, all named, no side effect.
- `internal/session/http.go` fan-out — remote participants cross through
  Client.ForwardToURL, resolving policy via h.opts.Fed.PolicyFor.

## The wiring gap (DF-CRIER-309)

cmd/server/main.go arms the registry handler (SetPeerPolicies) and the
session handler (SetPeerPolicies) but never the federation CLIENT
(fedClient.SetPeerPolicies). The fan-out resolves policy through the client →
nil → "peer not known". remote_test.go's armPeerPolicy calls
client.SetPeerPolicies by hand, so tests are green where production is dead.
Fix is one line + a boots-the-real-binary probe.

## Errors hit on the way (the honest trail)

1. `-db-url /tmp/x.db` → "failed to parse as keyword/value": the flag feeds a
   PostgreSQL DSN parser; there is no sqlite path behind it at HEAD. In-memory
   backends used for the run.
2. Port-hold silent restarts: `nohup ./crier -port 18767 ... &` over a live
   server exits with "address already in use" while the OLD process keeps
   serving — several "restarts" in the run did nothing until
   `./crier -stop -pidfile alpha.pid` was used. Lesson: grep the fresh boot
   log for `address already in use` before trusting any post-restart probe.
3. Hand-signed probes 401 while local verification passed: caused by the
   stale-port trap above (the OLD alpha had loaded the doc before key
   rotation... actually the doc was correct; the confusion came from testing
   against a process whose env predated edits). Resolved by clean restart +
   byte-exact transcript; python and openssl produce identical signatures.
4. Session create requires created_by as an object (`{\"agent\":\"x\"}`);
   a plain string fails with the Go type name in the error.

## The right way (verified recipe)

```
# destination (alpha)
CR_FED_AUTH_FILE=auth.json  # {"peers":[{"peer":"beta","public_key":"<64hex>"}],"revoked":[]}
CR_FED_PEERS_FILE=peers.json  # inbound walls; url only if IT forwards out
CR_FED_LINKS=http://localhost:18768   # needed even on the destination, or FEDERATION_UNCONFIGURED
# source (beta)
CR_FED_LINKS=http://localhost:18767 CR_FED_SELF_KEY_FILE=selfkey.pem
CR_FED_PEERS_FILE=...  # self_as: "beta" announces the identity alpha's gate proves
```
Signed hand-rolled forward: sign `POST\n/agents/alice/inbox\n<unix-seconds>`
with any ed25519 key, send X-Crier-Fed-Peer/X-Fed-Ts/X-Fed-Sig.
