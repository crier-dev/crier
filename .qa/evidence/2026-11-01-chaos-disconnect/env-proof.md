# env-proof.md — attempt-4 run environment verification (added 2026-11-01 by the foreman tick
# crier-foreman-2026-11-01-02-14-15, from probes run on the tick host against this repository)

The first judge verdict on this row (gitreins run 5dee7c2f against a20063a2) raised three evidence
questions. Each is settled below by a literal probe whose exact commands and outputs are recorded.

## 1. Was the suite run under the cut? (run-env contract)

The attempt-4 battery was launched with the fleet chaos cell's documented cut environment
(HTTP_PROXY=HTTPS_PROXY=ALL_PROXY=http://127.0.0.1:9, GOFLAGS=-mod=mod, GOPROXY=off, GOSUMDB=off,
GOTOOLCHAIN=local) — see the attempt-4 procedure record in agent.log and the COMMAND line in
disc.log. cut-proof.log (attempt-4-cut-proof) proves the proxy leg of the cut in the same agent:
CONTROL external fetch rc=0 without the env, the same fetch rc=7 through 127.0.0.1:9 with it.

## 2. Do `go: downloading` lines prove open egress? NO — probe transcript

Command (tick host, 2026-11-01): a fresh GOMODCACHE seeded with only three module zips
(testify, pgxmock, yaml), GOPROXY=off, GOSUMDB=off, GOTOOLCHAIN=local, `go test ./internal/registry
-run TestNothingMatchesThisName`. Observed output:

    go: downloading github.com/golang-migrate/migrate/v4 v4.20.1
    go: downloading github.com/gorilla/mux v1.8.1
    go: downloading github.com/jackc/pgx/v5 v5.11.0
    go: downloading modernc.org/sqlite v1.59.0
    go: downloading gopkg.in/yaml.v3 v3.0.1
    internal/permissions/postgres_store.go:9:2: module lookup disabled by GOPROXY=off
    internal/registry/migrate.go:10:2: module lookup disabled by GOPROXY=off

With GOPROXY=off the `go: downloading` verb prints for EVERY module the go command attempts to
materialize — including modules it then fails to resolve ("module lookup disabled by GOPROXY=off").
The verb is not evidence of network access. In disc.log the three downloading lines
(testify v1.12.1, pgxmock/v5 v5.2.0, go.yaml.in/yaml/v3 v3.0.5) name exactly the modules whose
zips were pre-staged into the agent's module cache (see agent.log staging records), and every
subsequent package compiled and ran — consistent with extraction from the seeded cache and
inconsistent with the lookup-disabled failure shape above.

## 3. Is the single suite red a "no cut" signal? NO — probe transcript (loopback proxy collateral)

Command (tick host, 2026-11-01): a REAL listener on 127.0.0.1:35355 (python3 -m http.server),
fetched by a Python urllib client:

    control (no proxy env):      FETCH-OK 200
    cut (dead proxy env):        FETCH-FAIL URLError <urlopen error [Errno 111] Connection refused>

A Python HTTP client under the dead-proxy env cannot reach a REAL, LISTENING loopback server and
fails with the same "[Errno 111] Connection refused" text family recorded in disc.log for
TestServiceDeliveryPythonInterop (internal/registry). Go net/http bypasses loopback proxies by
default; Python urllib does not. The red is therefore loopback-proxy collateral of the strict
all-proxy cut (tracked as QA-CRIER-53), not a server-startup failure and not evidence of a missing
cut. The cell's own question — clean fail or HANG — answers clean fail-fast.

## Conclusion

The battery ran on a fresh clean JIT agent (qa49-batt2-20261101) with the repository suite
available (offline build rc=0, see README attempt 4) under the verified dead-proxy cut, and the
evidence names the command and the result (disc.log: COMMAND + RESULT lines, non-empty). The single
red is characterized above and tracked in QA-CRIER-53. Criterion 1 is evidenced; criterion 2's
regression (internal/ci TestChaosDisconnectEvidenceContract) is green on the landed tree.
