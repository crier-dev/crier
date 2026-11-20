# Judge failover — gitreins tier-2 in this repo (INT-CI-042)

## State (measured 2026-11-20)

| Lane | Endpoint / model | Status |
|---|---|---|
| primary | `deepseek-v4-flash` @ `https://api.deepseek.com/v1`, key `GITREINS_LLM_API_KEY` | **DEAD.** Every tier-2 call 402'd `Payment Required` (runs e0f88d05, e304a6a7, a8911b7d, verdicts 6f1ef4cf, 4db89d35, 77c3cbeb). Re-probe 2026-11-20: the key now 401s `Authentication Fails ... invalid` at `/user/balance` and `/chat/completions` — rotated/revoked, not just unfunded. |
| ambient env unit | `~/.hermes/.env` | **MISWIRED.** `GITREINS_LLM_BASE_URL` points at fireworks but `GITREINS_LLM_API_KEY` holds the dead deepseek key (25 chars) — a bare `gitreins` call sends the deepseek key to fireworks and 401s. |
| fallback (armed) | `accounts/fireworks/models/kimi-k3` @ `https://api.fireworks.ai/inference/v1`, key `FIREWORKS_KEY_GITREINS` | **ALIVE.** 2026-11-20: `/models` → 200 with kimi-k3 listed; chat completion → 200 (reasoning model, alive and answering). |

## Required invocation path

```bash
scripts/gitreins-judge-lane.sh task complete <TASK-ID>          # tier1+tier2 on the fallback lane
scripts/gitreins-judge-lane.sh judge <TASK-ID> --async          # large tiers / re-grades
scripts/gitreins-judge-lane.sh task complete <id> --skip-tier2  # tier1-only close for mechanical criteria
scripts/gitreins-judge-lane.sh --lane primary <args...>         # retry deepseek once re-funded/rotated
```

The wrapper resolves the key internally from `~/.hermes/.env` (never argv, never
echoed), length-guards it (a short key wedges the judge as a silent hang), and
exports base URL + model + key as ONE consistent unit.

Do NOT "fix" this by exporting `GITREINS_LLM_*` into `~/.hermes/.env` or a shell
profile: the ambient env is host-shared, and the wrapper is the only place the
unit is guaranteed consistent.

## Verdicts

Verdicts land under `.gitreins/history/<date>/<run-id>/verdict.json` (gitignored,
host-local). Locate a task's verdict by grepping the TASK ID across that day's
dirs and matching `.task_id` — the printed `Verdict saved: <8hex>` id is NOT the
directory name and may resolve to no file at all.

## Proof this lane grades (the AC's live evidence)

Filled in by the closing tick: see the INT-CI-042 board row for the
`DF-CRIER-505` re-grade verdict id (the row's proof command) and the wrapper's
own task verdict.
