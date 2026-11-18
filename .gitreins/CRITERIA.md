# Criterion authoring for the crier tier-2 judge

**Status:** enforced. `criteria.enforce: refuse` in `.gitreins/config.yaml` refuses a
`gitreins task create` whose criterion is thin (`--allow-thin` downgrades it to a
warning for one call).

## Why this file exists (measured, not asserted)

Board row QA-CRIER-064: "gitreins tier-2 judge for crier cannot reach its criterion:
3 of 5 runs ended UNEXAMINED at the iteration/size cap". Every starred-out run below
spent its **entire** iteration budget and graded **0 criteria**, and in every case the
recorded tool calls show the judge scrolling an evidence set one line per call instead
of grading — not a run that needed a little more room:

| run | task | cap line from verdict.json | graded | tool calls | what the judge actually did |
|---|---|---|---|---|---|
| `2026-11-13/06fb79f8` | QA-CRIER-58 | `Cap exceeded: Iteration cap (600) reached (600.4 used). … 0 of 1 criteria graded` | 0/1 | 200 recorded + **735 omitted** | 140 distinct `grep -o … \| sed -n 701p`, `702p`, … one match per call |
| `2026-11-13/a503d646` | QA-CRIER-60 | `Iteration cap (600) reached (600.9 used) … 0 of 1 criteria graded` | 0/1 | 200 + **360 omitted** | the *same* command re-issued **166 times** verbatim |
| `2026-11-13/0f0e80e1` | QA-CRIER-57 | `Iteration cap (600) reached (600.7 used) … 0 of 1 criteria graded` | 0/1 | 200 + **348 omitted** | `git show 42f193ca:…/handler_test.go \| sed -n '532,542p'` then `533,543` … one line per call |
| `2026-11-16/92778507` | QA-CRIER-57 (re-run) | `Iteration cap (600) reached (600.2 used) … 0 of 1 criteria graded` | 0/1 | 200 + **353 omitted** | `git show 7b2f0d27 -- … \| sed -n '476p'`, `477p`, … one line per call |

Zero `go test` invocations appear in any of those runs' tool records. The judge never
reached the gate; it was paging a search result set that no rung can exhaust.

The cap history in `.gitreins/config.yaml` is four rungs of the same experiment
(100 → 250 → 400 → 600, plus 9M → 12M → 16M → 24M → 48M tokens). Each raise was
justified by "exact-equality starvation" on a run that had a **complete evidence set**.
That reasoning does not hold for the four runs above: they starve with **no** evidence
set because the criterion never says what evidence would settle it. A larger rung buys
more scrolling, which is why the same signature came back at every rung.

## The rules

A criterion is the judge's entire work order. It has to be **bounded**: the judge must
be able to name the evidence set and stop.

1. **Carry `PROOF:` and `FAILIF:`.** `PROOF:` names a locator (`path`, `path:line`,
   `tests/x.py::test_y`) or a command that runs inside the judge's command tool. `FAILIF:`
   is the condition that makes the criterion fail. Enforced by `criteria.enforce: refuse`.

2. **Resolve every noun against the tree.** "clean and chaos-resource legs" is
   bunker-QA cell vocabulary: the term appears nowhere in the repo except dogfood
   narrative, and the judge answered it the only way it could — by grepping for the
   string and paging 140 hits from a commit patch. Say `go test -count=1 ./internal/federation/`
   and name the cell's command if a cell is what proved it.

3. **Never write an enumeration as a search obligation.** "all three failures" makes the
   judge find the set. Enumerate them: entry point, file, line. A named list is graded;
   an unnamed set is paged.

4. **Keep the gate inside the tool's command budget, or cite a file.** The judge's
   `run_command` is hard-capped at 30s (tool-side; tracked as DF-CRIER-506). A gate that
   fits (`go test -count=1 ./internal/federation/` is ~4s) belongs in `PROOF:`. A gate that
   cannot fit belongs in a committed evidence artifact that `PROOF:` points the judge at
   by path — never as a command the judge is required to run and cannot.

5. **Split what fails independently.** One criterion per independently-failable claim
   (own task when the wave creates rows): a graded FAIL on claim A must not hide whether
   claim B was ever examined. A compound criterion is fine when its claims share one
   evidence set — then say so and bound it.

6. **A criterion that names a gap must name it as a gap.** "reproduced at fixed SHA under
   stress" is only gradable if the SHA and the stress command are in the criterion, or if
   a committed evidence file records the attempt and the outcome. `.coding-hermes/evidence/<row>.md`
   is the established home; cite it.

## Re-judging an existing row

`gitreins task` has no edit verb, so a criterion is changed in `.gitreins/tasks.yaml`
(tracked) directly — block scalar under `criteria:` — then:

```sh
gitreins task start <id>      # re-opens the row for grading
gitreins task complete <id>   # grades the CURRENT tree, writes
                              # .gitreins/history/<date>/<run>/verdict.json
```

Run judge runs **sequentially**: two concurrent tier-2 runs in one checkout collide on
tier-1 coverage artifacts and spuriously FAIL. Read `examined` from the per-criterion
`items[]` of the history verdict (`status` `PASS` / `FAIL` / `UNVERIFIED` / `NOT_VERIFIABLE`);
`UNVERIFIED` + `detail: "Not examined — evaluation ended before this criterion was reached"`
is the failure this file exists to prevent.
