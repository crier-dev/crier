# Crier dogfood diagnostics — 2026-11-27

Use the first-party route for a real Crier smoke:

1. Build the actual server entrypoint: `go build -o bin/crier ./cmd/server` or `make build`.
2. Generate PKCS#8 Ed25519 keys with `./bin/crier keygen -out <key> -id <id>`; no OpenSSL, xxd, or package manager is required.
3. Start a scratch server with `CR_REQUIRE_AGENT_SIG=true` and `CR_GUARD_ENABLED=false` on a free port. The server's `/version` response is the provenance check.
4. Run `python3 clients/python/round_trip.py` and `python3 clients/python/two_agents.py`; run `node clients/typescript/round-trip.ts` when Node 22.6+ is available.

Observed at 0db42bf6:

- Python and TypeScript signed round-trips both completed all 12 steps, including the negative controls. Cross-agent ownership is enforced: a foreign ack returns 403, and unsigned delivery returns 401 without storing the message.
- The scratch server's default no-database mode is explicitly non-durable (`registry_backend=memory`, `registry_durable=false`). Treat restart persistence as a separate PostgreSQL/SQLite arm, not as implied by a successful message round-trip.
- Node emitted MODULE_TYPELESS_PACKAGE_JSON because the repository has no package.json declaring module type. This is warning-only in the tested flow but adds avoidable onboarding noise.
- On the fresh bunker, the full native suite exposed DF-CRIER-518's pidfile-test false red: `MismatchError.Live = "/tmp/go-build.../pidfile.test", want it to name the real binary (sleep)`. The product safety refusal itself held; the test's process identity expectation did not.
- The fresh bunker act simulation failed before a trustworthy product verdict because the simulated hosted Go toolchain paths were absent. Keep this as UNVERIFIED, not as a Crier product failure.

Performance measurement:

- Python cold round-trip: 0.71 s, rc=0.
- Python warm round-trip: 0.67 s, rc=0.
- Two-agent flow: 0.31 s, rc=0.
- Build: 2.97 s.
- Host load was about 16.15–16.77 during the client timings, so these are host-contended observations, not quiet-host capacity claims. The successful operations were below a user-noticeable threshold and no profile was justified.

Install evidence:

- Agent: 98a7292e on bunker-las-02.
- Tested commit: 0db42bf65588563c07596421d8db3afd8db58f31.
- Evidence digest: 1e19ab344d6383d9ea7e070b363ab2fc481b876552bbb660fd8521b0a79a17ef.
- Persisted evidence directory: /home/kara/.hermes/qa/evidence/crier/qa-20261127T212021Z.
- The collector completed, but the named agent remained listed after a 240-second destroy attempt. Teardown is pending/unverified.
