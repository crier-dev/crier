# crier dogfood evidence — 2026-11-27

Measured tree: 0db42bf65588563c07596421d8db3afd8db58f31.

Files in this packet:

- local-run.sh — staged, reproducible scratch-server runner used for the Python, two-agent, and provenance arms.
- typescript-run.sh — staged runner used for the Node/TypeScript client arm.
- local/ — captured build, server, version, status, client stdout/stderr, and timing files.
- bunker/evidence.jsonl — merged bunker-qa evidence from the fresh-machine install leg; local SHA256 1e19ab344d6383d9ea7e070b363ab2fc481b876552bbb660fd8521b0a79a17ef.
- integration-report.md — explained user journey and measured verdict.
- diagnostics.md — reusable reproduction notes and boundaries.

The local server used a unique scratch port and was stopped by its own pidfile path. No credentials or private keys are included in this packet. The bunker agent was named explicitly in the run record; evidence collection completed, while teardown remained pending after the destroy timeout.
