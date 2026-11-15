# Install-leg evidence — crier dogfood tick 2026-11-15-15-54-49

What this directory holds: everything that SURVIVED from the ephemeral-bunker
install leg of this tick. It is deliberately incomplete; the missing half is the
finding (see `docs/dogfood/diagnostics-2026-11-15-ci-act.md` and board row
QA-CRIER-066-R2).

## Files

| file | origin | note |
|---|---|---|
| `harness-evidence.jsonl` | `/tmp/bunker-qa-evidence-20261115T161247Z-628046.jsonl` | 3 harness rows only: launch OK, collect FAIL, harness abort. sha256 `2625bb8a8c4ede1e65e50cd285a15b8e662f00fd28cb45cba005903905dde597` |
| `launch.meta.json` | same basename + `.meta` | agent 765c30b7 / server bunker-las-03 / project crier |
| `launch.spawn.redacted.json` | same basename + `.agent-765c30b7.spawn` | agent API key + agent tailnet IP REDACTED before commit (the original was world-readable 0664 — that is QA-CRIER-065, still pending) |
| `act-ci-online.log` | this fire's local act reproduction | full step stream, cold cache, PASS (test 0.057 s) |
| `act-ci-offline.log` | this fire's local act reproduction | `--network none`, FAIL in 0.127 s with the dial error captured |
| `act-ci-warmcache-mem2g.log` | this fire's local act reproduction | warm actions cache under `--memory 2g --memory-swap 2g`, PASS (step 0.85 s) |
| `act-repro.workflow.yml` | this fire's local act reproduction | the minimal workflow used |

## Launch (16:15:19Z)

    agent=765c30b7 server=bunker-las-03 ttl=4h cpu=4.0 script_bytes=138601 project=crier

## Collect (16:42:32.987Z → 16:42:33.267Z, 273 ms)

    cell=collect status=FAIL
    cannot reach agent=765c30b7 on bunker-las-03 (ssh probe returned no state) — agent left intact

Immediate fail-closed probe (273 ms), not a timeout. The harness aborted after a
single collect attempt; there is no retry pass in the record.

## What is NOT here, and why it cannot be recovered

The run's own ledger (26 cells) and `qa-logs-286493/ci.log` (7742 B) lived only on
agent 765c30b7 and were harvested by hand with `bunker exec` because `collect`
aborted. They were never written into the repository. `bunker list` on
bunker-las-03 now shows one unrelated agent (3c9c4402, created 11:09 EST);
765c30b7 is gone, so those bytes have no surviving copy anywhere.

Consequence: DF-CRIER-497-R2 cites an artifact no reader can open, and its
acceptance criterion "a red act step can be attributed from the evidence alone"
is now unsatisfiable for that run.

## Redaction note (why this matters beyond this directory)

The agent API key in the spawn bundle is **opaque** — it carries no `sk-` prefix, so a
secret scanner keyed on provider prefixes (gitleaks' `sk-` rules included) will not flag
it. The first redaction pass here was a `sk-[A-Za-z0-9_-]{6,}` substitution and it changed
**nothing**; the key only went away on a second, explicit pass over the `API Key:` line.
That is a concrete argument for QA-CRIER-065 being more serious than "a file had 0664
permissions": the standard scanner sweep cannot see this class of credential at all.

Read-back check for anyone verifying this file:

    sha256sum .coding-hermes/evidence/crier-dogfood-2026-11-15-15-54-49/harness-evidence.jsonl
    # 2625bb8a8c4ede1e65e50cd285a15b8e662f00fd28cb45cba005903905dde597
