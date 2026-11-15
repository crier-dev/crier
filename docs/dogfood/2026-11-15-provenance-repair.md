# Provenance repair + ledger census — crier dogfood tick 2026-11-15-15-54-49 (fire 4)

Fire 4 of this tick re-ran nothing: the product tree is byte-identical to fire 1's
measurement pin, so every earlier number stands. Its material work was a **ledger
integrity repair** — two rows filed by earlier fires of this same tick existed on the
board with **no `task_created` event**, i.e. work a foreman can see but an auditor
cannot trace.

## What was wrong

| row | filed by | commit | `task_created` events before | after |
|---|---|---|---|---|
| `DF-CRIER-497-R2` | fire 1 (2026-11-15a) | `9d0240f4` | 0 | 1 (event `1794692633205781843`) |
| `QA-CRIER-066-R2` | fire 2 (2026-11-15b) | `c859600a` | 0 | 1 (event `1794692633205781844`) |

Both rows were written with `~/.hermes/scripts/board_append.py`, which is a **row-only
appender**: it appends to `tasks.jsonl` and never emits an event. Any lane that files
through it mints untraceable rows. Fire 3 avoided it (it wrote its own events), which is
why its two rows were paired and these two were not.

Repair was an append-only provenance write through the canonical writer:

```
boardctl -C . event --type task_created --task-id <id> --actor dogfood --no-tick-write \
  --detail-text '{"id":"<id>","status":"pending","repair":"provenance backfill", ...}'
```

Verification: `git diff --numstat` = `2  0  .coding-hermes/board/events.jsonl` and
**nothing** for `tasks.jsonl`; per-id `task_created` count 0 → 1 each; no row's status,
priority or body touched; `boardctl validate -C .coding-hermes/board` = `RESULT: OK
(449 warning(s), 0 errors)` — unchanged warning count, no new diagnostics.

## Board-wide census (context, not this tick's defect)

- rows: 954, distinct ids: 954, duplicate task ids: none
- rows carrying a `task_created` event: 443
- rows lacking one: **511** (legacy, ~53% of the board)
- of those lacking one, filed since 2026-11-10: `QA-CRIER-57`, `QA-CRIER-REVIEW-7-R2`,
  `RELEASE-030`, `QA-CRIER-066` — the long tail that predates the event-emitting writers

This is board tooling, not the crier product, so it is **reported here and not filed as a
crier row**; the fix belongs to the fleet writers (`boardctl create`/`event`), and the
row-only helper should gain an event leg or be retired.

## Arithmetic corrected in the tick record

Fire 3 wrote "board 952→954 rows". Measured: `a065ad2a` = 952 rows, `c859600a` = 953
(fire 2's one row), `9df1476c` = 955 (fire 3's two rows). So fire 3's row delta was
953→955, not 952→954; its event delta (2573→2575) was correct. At HEAD before this
fire's repair: 954 rows / 2575 events (the 954th row is a later sibling append, which is
why the row count and the fire-3 delta differ).
