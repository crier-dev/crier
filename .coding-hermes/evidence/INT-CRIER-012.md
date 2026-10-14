# INT-CRIER-012 — bunker-e2e CI red: bunkerd SpawnAgent dial :10001 i/o timeout

## Root cause (two independent failures, both fixed 2026-10-14)

1. **Self-hosted runner `kara-main` (labels: self-hosted, Linux, X64, bunker) was OFFLINE since 2026-09-28.**
   systemd unit `actions.runner.crier-dev-crier.kara-main.service` failed: the runner died on
   `System.IO.IOException: No space left on device` writing its `_diag` log (disk was 100% on
   2026-09-28). Jobs queued forever; the scheduled bunker-e2e runs never started a runner.
   Fix: pruned `_diag` logs to the 10 most recent (92M total; disk now 391G free) and restarted
   the unit — runner verified ONLINE and busy via the runners API.

2. **`BUNKER_SERVER` secret pointed at a dead endpoint (:10001 — dial tcp i/o timeout), and
   `BUNKER_HOST` pointed at a host whose ssh transport refuses scp.** Reachability probe from the
   tailnet: bunker-las-01/02/03/04 all dead (000); only `bunker-mvp` (100.115.67.116:18080,
   bunkerd active, pid 3210367) and cube-las-00 answer.
   Fix: repo secrets repointed (verified `gh secret set` OK):
   - `BUNKER_SERVER` = `bunker-mvp` (the bunker CLI resolves `--server` by NAME from the runner
     user's `~/.bunker/config.yaml`; a raw URL fails with `server "…" not found in config`)
   - `BUNKER_HOST` = `100.115.67.116` (bunker-deploy.sh scp's the image tarball to
     `bunker-$AGENT@$HOST`, so this must be the agent host itself)

## Verification

- First attempt with only the runner + BUNKER_SERVER fix (run 38566899491): `Ensure bunker agent
  exists + heartbeat` step PASSED, then the matrix deploy failed on scp `Connection closed` —
  which exposed the BUNKER_HOST gap (root cause 2) via transport-retry VERDICT lines.
- After the BUNKER_HOST repoint, dispatch run **38574167771**:
  - job "Agent-ecosystem battery (compose stack)": success
  - job "Bunker config-matrix E2E": **success** (both Ensure and Deploy steps green)

## Scope note

No crier code changed. Fixes are operational: runner service restart + GitHub repo secrets
(secret values are not readable back through the API; the `gh secret set` receipts and the green
run above are the evidence). Criteria "BUNKER_* repointed and one full run green" met.
