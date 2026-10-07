# Release/deployment acceptance matrix (CR-CHAT-037)

`scripts/acceptance-matrix.sh` proves the comms/federation flows a release
ships against the BUILT artifacts — the binaries `make build` / `make
build-mcp` produce and the image `Dockerfile` builds — not against unit tests
on source. It is a CI job (`acceptance-matrix` in `.github/workflows/ci.yml`,
after `build`) and runs locally with no arguments.

## Cells

| Cell | What it proves | Against |
| --- | --- | --- |
| 1-artifact-identity | `bin/crier` and `bin/crier-mcp` answer `-version` with a non-empty build identity | built binaries |
| 2a-sqlite-relaunch | register → deliver → **signed** retrieve (ed25519 `X-Agent-ID/Ts/Sig`, transcript `METHOD\nPATH\nTS`) → ack round-trip; then the process is KILLED and relaunched on the same sqlite data dir and the session view is read back — relaunch persistence is the durability proof | built server binary, sqlite session backend |
| 2b-postgres | register → deliver → retrieve against a scratch `postgres:16-alpine` (`CR_DATABASE_URL`); migrations apply on a fresh database | built server binary + real PostgreSQL |
| 3-session-threads | `GET /chat`, `POST /sessions` (object `created_by`), `GET /sessions/{id}/output?mode=trace` and `?mode=summary`; a session with no threads answers the documented 404 `SUMMARY_UNAVAILABLE` — never a synthesised summary | built server binary |
| 4-federation-peer-auth | the destination runs the CR-CHAT-024 peer-auth gate (`CR_FED_AUTH_FILE`): a peer announcing `X-Crier-Fed-Peer` with a VALID `X-Fed-Ts`/`X-Fed-Sig` ed25519 signature over `FedAuthPayload(method, path, ts)` is delivered (201); the same key over a WRONG transcript is refused (401) | two crier instances (peer-auth gate armed on the destination) |
| 5-docker-image | the image `Dockerfile` builds is `docker run` on a scratch port and answers `/health`; the container is removed on teardown | built Docker image |

## Semantics

- One `PASS <cell>: <detail>` / `FAIL` / `SKIP` line per cell; the runner exits
  0 only when every non-SKIP cell passed. The summary line is always printed.
- SKIP is recorded WITH A REASON and only for infrastructure the runner does
  not control (docker daemon down). The sqlite relaunch cell never skips.
- Process hygiene (CR-GAP-069): every spawned crier is backgrounded, registered
  in the EXIT trap (bounded grace, then SIGKILL), and asserted to OWN its port
  via `scripts/lib/port-guard.sh` (`require_free_port` + `assert_port_owned` +
  `wait_http_or_die` — a server that dies before answering aborts the run, it
  is never papered over). Scratch ports rotate past occupied candidates.

## Environment

bash, curl, OpenSSL >= 3 (`pkeyutl -sign -rawin` with a seekable payload file —
a piped payload fails with "unable to determine file size for oneshot
operation"), python3, `ss` (iproute2), docker (optional — absence is a
recorded SKIP). `SKIP_DOCKER=1` pre-skips the docker cells.

## Run

```sh
scripts/acceptance-matrix.sh
SKIP_DOCKER=1 scripts/acceptance-matrix.sh   # skip docker cells up front
```
