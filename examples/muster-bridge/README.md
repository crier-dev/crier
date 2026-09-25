# muster-bridge — the "Using crier from Muster" onboarding path, runnable

Muster is an external platform that generates an HTTP client from an OpenAPI
document. Crier ships that document (`docs/openapi.yaml`, served at
`/openapi.json` and `/openapi.yaml`), so a Muster-driven integration is built
from the spec — no Go, no SDK, no WebSocket client.

`run-demo.sh` walks that onboarding path against a relay it builds and starts
itself, and **asserts every status it prints** (`register → deliver → retrieve →
ack`, plus the full authorization matrix). Nothing in its output is transcribed:
each line is a live measurement compared against the documented code, and a
mismatch fails the run.

```bash
bash examples/muster-bridge/run-demo.sh                          # config C (the production default)
MUSTER_BRIDGE_REQUIRE_SIG=false bash examples/muster-bridge/run-demo.sh   # config B (bearer only)
```

## What it does, step by step

| Step | Action | Measured |
|------|--------|----------|
| 3/9 | discover the spec — the two served documents | `GET /openapi.json` → 200, `GET /openapi.yaml` → 200 (both auth-exempt: a spec fetch needs no token) |
| 4/9 | register the consumer with its ed25519 public key | `POST /agents` → 201 **unsigned**; without `public_key` → 400 `{"error":"public_key is required"}` in config C |
| 5/9 | deliver two messages into its inbox | `POST /agents/{id}/inbox` → 201, 201 **unsigned** |
| 6/9 | retrieve — `GET /agents/{id}/inbox` | config C: **401** unsigned (`missing agent signature headers`); config B: 200 with the batch |
| 7/9 | sign the request — the leg no generated client can do | signed retrieve → 200, with the repo's OpenSSL 3 helper over `"METHOD\n<path>\n<unix-seconds>"` |
| 8/9 | ack the lease, then drain-check | signed `POST /agents/{id}/inbox/ack` → 204, `GET /agents/{id}/inbox/stats` → 200 |
| 9/9 | the measured authorization matrix | every row below, asserted |

## The measured authorization matrix (config C, bearer-only client)

`Authorization: Bearer <token>` only — no `X-Agent-ID` / `X-Agent-Ts` /
`X-Agent-Sig`:

| Endpoint | Unsigned | With the signature |
|----------|----------|--------------------|
| `GET /health`, `GET /version`, `GET /openapi.json`, `GET /openapi.yaml`, `GET /docs` | 200 — no token needed at all | — |
| `GET /status` | 200 (401 without the token) | — |
| `GET /agents`, `GET /agents/{id}` | 200 (401 without the token) | — |
| `POST /agents` | 201 | — |
| `POST /agents/{id}/inbox` (deliver) | 201 — no signature in any config | — |
| `GET /relay/topics`, `GET /mesh/peers`, `GET /fed/peers` | 200 | — |
| `POST /relay/publish` | 202 | — |
| `GET /agents/{id}/inbox` (retrieve) | **401** | 200 |
| `GET /agents/{id}/inbox/stats` | **401** | 200 |
| `POST /agents/{id}/inbox/ack` | **401** | 204 |
| `PATCH /agents/{id}` | **401** | 200 |
| `DELETE /agents/{id}` | **401** | 204 |

The spec and the live server agree on that split, including
`GET /agents/{id}/inbox/stats`: `docs/openapi.yaml` declares `agentSignature` on
it and an unsigned call is refused with 401 (INT-MUSTER-003 was filed because an
earlier note believed that operation was unsigned — it is not).

With `MUSTER_BRIDGE_REQUIRE_SIG=false` (config B) the same matrix is measured
with enforcement off: the five signature-required operations answer 200 / 200 /
404 (a bogus message id, which is the ack path doing its job rather than the auth
gate) / 200 / 204, and a bogus signature trio is ignored rather than rejected —
so config B is the configuration a bearer-only generated client drives end to
end.

## Why the signature leg is not generated

`agentSignature` is declared in the spec as an apiKey-style header
(`X-Agent-Sig`), but its value is a per-request ed25519 signature over
`"<METHOD>\n<path>\n<unix-seconds>"` with the agent's **private** key. A client
generated from the spec can set headers; it cannot sign with a key it does not
have, and the server binds the signature to the method, the path and the
`X-Agent-ID` (a signature minted for one path is refused on another; a
signature presented as a different agent is refused 403). That is why step 7/9
uses the repo's documented OpenSSL 3 helper — see the integration guide §3
(signing helper) and §10 (this path), and `examples/demo.sh` for the same
helper in the register/deliver/retrieve/ack round trip.

## Requirements and knobs

Requirements: `go` (toolchain only), `curl`, `openssl` 3+ (`pkeyutl -sign
-rawin`), `xxd`, `ss` (iproute2 — how the port guard finds the listener),
`sed`/`grep`.

| Env var | Default | Meaning |
|---------|---------|---------|
| `MUSTER_BRIDGE_REQUIRE_SIG` | `true` | `true` = config C (the production default); `false` = config B, the whole round trip unsigned |
| `MUSTER_BRIDGE_TOKEN` | `muster-bridge-token` | the bearer token the relay is started with (an ambient token is ignored) |
| `MUSTER_BRIDGE_AGENT` | `muster-bridge` | the consumer id this run registers |
| `MUSTER_BRIDGE_PORT` | first free of `MUSTER_BRIDGE_PORT_BASE`+ | use THIS port; an occupied one aborts naming its holder, never rotates |
| `MUSTER_BRIDGE_PORT_BASE` | `18801` | first candidate of the scratch-port rotation |
| `MUSTER_BRIDGE_PORT_CANDIDATES` | `5` | how many candidates the rotation may try; all occupied is a named failure |
| `MUSTER_BRIDGE_TRANSCRIPT` | `mktemp` under `$TMPDIR` | where the transcript is written (nothing is written into the repo) |

The run is `CR_GUARD_ENABLED=false` (no `DEEPSEEK_API_KEY` and no provider call
in the loop — see the integration guide's LLM message guard section), and the
backend is in-memory (`CR_DATABASE_URL` unset).

## Cleanup contract

The script spawns the relay in the background, so CR-GAP-069 applies and is
enforced by `make demo-cleanup-check`: it registers an `EXIT` trap that kills
**the pid it started**, and after the start it asserts that the pid **holding the
port** is that pid (`assert_port_owned` from `scripts/lib/port-guard.sh`). A
squatter on the scratch port can neither be measured nor left running.
