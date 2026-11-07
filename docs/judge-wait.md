# Waiting on a long command in the tier-2 judge

The GitReins tier-2 judge is an LLM that grades this repository's diff with a small
tool set: `read_file`, `read_diff`, `search_pattern`, `run_command`, `sandbox`.
Everything it does is bounded by a tool budget, and `run_command` is the tightest
bound in the set.

This page records the ceiling, the failure it has twice produced, the wait
primitive that replaces the failure, and the one thing that is still upstream's
to fix.

## 1. The ceiling: `run_command` gets 30 seconds

The judge's `run_command` tool is wired with a hard-coded 30-second timeout:

    # gitreins engine/evaluator.py
    command_timeout: int = 30

It is a dataclass default, never passed in from a configuration file. Measured on
this host while writing this page (2026-11-07): the installed gitreins is 0.17.1
(pipx), the latest available is 0.18.1, and **neither** release exposes a config
key for it — `gitreins` reads no `command_timeout`, no
`evaluator.command_timeout`, and no per-task override. The same 30 seconds apply
to every `run_command` call in a judge run, on every repository.

`go test -short ./...` on crier does not finish in 30 seconds. So a judge that
wants to run the suite has exactly two honest options:

1. run it in the background, then **wait** on recorded evidence that it finished;
2. do not run the suite at all, and grade the tree instead (see §4).

What it must not do is invent a poll loop around the process table.

## 2. The failure shape: a probe that matches itself

Twice the invented loop has been a `pgrep -f` scan wrapped in a shell that also
carried the pattern. The observed shape (this is the wrapper _shape_ observed
live in the tick — the judge's command transcript is not persisted in
`.gitreins/history/*`, which keeps only `commit.patch`, `summary.md`,
`verdict.json` and `worktree.patch`, so no verbatim argv is quoted here):

    sh -c 'while pgrep -f "make test-short" >/dev/null; do sleep 5; done'

`pgrep -f` matches against the full command line of every process. The wrapper's
own command line contains the string `make test-short`. The wrapper therefore
matches **itself**, the loop condition is true forever, and the wait can never
observe the completion it was written to observe — no matter how long it runs.

Two dated occurrences:

| Date | Row | What was measured |
| --- | --- | --- |
| 2026-11-03 | judge run on the RELENG lane | the loop was still reporting RUNNING after the suite had finished; killed by exact PID, then re-judged via `gitreins judge --async` |
| 2026-11-07 | RELEASE-015 @ `08a54216` | the suite logged completion at ~19:00Z while the loop still reported RUNNING; the judge was killed at 28 minutes and wrote no verdict at all — `verdict=none:judge-hang` |

The cost is not the hung loop. It is that a run which consumed the whole judge
budget produces **no verdict**, so the work it was grading is unjudged and the
tick has to be repeated. `make wait-for-command-selftest` reproduces this trap
on the host, in the same process tree, on every run: it launches a job whose
argv carries a unique per-run token, lets it exit, then shows that
`sh -c 'pgrep -f "<token>"'` **still** reports a match — the probe's own argv —
while the marker-anchored waiter reports `STATUS=done`.

## 3. The blessed primitive

`scripts/wait-for-command.sh` waits on recorded data only. It has no name or
pattern mode at all, and a name-shaped request is refused with the self-match
reason rather than reinterpreted.

Two anchors, one mode each.

**Anchor on a PID you recorded.** Start the job, keep its pid, wait on that pid.
Liveness is read from `/proc/<pid>` plus `kill -0`; a zombie counts as exited
(it has exited — only its parent's reap is outstanding). `--expect-cmdline`
additionally refuses the wait if `/proc/<pid>/cmdline` no longer contains the
expected command, which is how a reused pid is caught instead of waited on:

    make test-short >/tmp/crier-test-short.log 2>&1 &
    job=$!
    scripts/wait-for-command.sh --pid "$job" --timeout 900 \
        --expect-cmdline 'make test-short'
    # STATUS=done pid=12345 elapsed=412 mode=pid

    # or, when the pid was written to a file by another process:
    scripts/wait-for-command.sh --pid-file /tmp/crier-suite.pid --timeout 900

**Anchor on the job's own completion line.** The job writes a marker when it is
done; the waiter greps that one file. Nothing else is read, and nothing about the
process table is consulted:

    { make test-short; echo "SUITE_DONE rc=$?"; } >/tmp/crier-test-short.log 2>&1 &
    scripts/wait-for-command.sh --log /tmp/crier-test-short.log \
        --marker '^SUITE_DONE rc=0$' --timeout 900
    # STATUS=done pid=- elapsed=417 mode=marker

A marker that carries the exit status (`SUITE_DONE rc=$?`) is the better anchor:
`--marker '^SUITE_DONE rc=0$'` reports done only on a **passing** suite, and a
failing run ends in `STATUS=timeout` at the bound instead of a false done.

Contract, in both modes:

- one line on stdout when it finishes — `STATUS=done|timeout pid=<pid|-> elapsed=<s> mode=<pid|marker>`;
- exit 0 done, exit 4 timeout, exit 2 refused (bad/conflicting arguments, a
  name-shaped request, an unbounded or above-cap `--timeout`, an unreadable pid
  file, a pid identity mismatch);
- `--timeout` defaults to 600s and anything above 3600s is **refused**, not
  clamped — a wait longer than an hour belongs to a supervisor;
- `--interval` (default 2s, max 60s) is the poll cadence and is capped by the
  time remaining, so the bound holds to within a second. No busy-wait.

`make wait-for-command-selftest` proves all of it (24 checks, ~7s, hermetic:
no network, no docker, no server, no fixed port) plus the refusals and a source
rule that the tool contains no pattern-search invocation at all, so the rule
cannot drift back in unnoticed.

## 4. Tier-2 criteria for a docs-only row should be tree-readable

A docs/board-only diff has nothing in it that running the suite could resolve, and
a judge that tries anyway burns its budget on a 30-second tool and a wait it has
to improvise. Two things follow.

**Write criteria the tree can answer.** For a docs or board row, every criterion
should be decidable by `read_file`, `read_diff`, `search_pattern` or a
sub-second `run_command` — "the file contains this section", "this deleted
sentence is gone from every doc that carried it", "this count matches the file it
counts", "`bash -n` passes on these two scripts". Criteria of the shape "the
suite is green" on a diff that cannot change the suite are what push a judge
toward a long-running command in the first place.

**Where the diff is genuinely non-source, skip tier 2 rather than pay for it.**
That is the existing, gated route: `scripts/lib/judge-diff-class.sh` classifies
the path list and authorizes `gitreins task complete <id> --skip-tier2` only for
a diff with zero source files, and `docs/ops-evidence.md` records the contract.
Measured tick 365: two tier-2 runs over three board JSONL files and zero source
files consumed 33.8M and 48.1M `tokens_in` against 188K-706K for a normal
source-diff judge, and the second died at the input cap. A docs-only row that
does keep tier 2 should expect the judge to read the tree, not to run the world.

## 5. Upstream handoff — not fixed in 0.18.1

The durable crier-side fix is what shipped here: a blessed, tested wait primitive
plus the written rule in `AGENTS.md`. What crier cannot fix from inside its own
tree is the general case.

Measured 2026-11-07: gitreins `engine/command_hygiene.py` refuses CPU busy-waits,
fork bombs, unbounded `yes`, `/dev/zero` burns and `dd` burns — its refusal policy
has **no self-match rule** — and the 30-second `command_timeout` is not
configurable in either 0.17.1 or 0.18.1. So a judge on any repository can still
write the self-matching poll loop, and any repository with a suite slower than 30
seconds still cannot run that suite inside one `run_command` call.

The upstream ask is therefore two changes in gitreins, and neither is made here:

1. a self-match refusal in `engine/command_hygiene.py` — reject a
   `pgrep -f` / `pkill -f` (any process-name pattern scan) whose pattern also
   occurs in the command line being executed, because the wrapper matches itself;
2. a configurable `command_timeout` (or a first-class bounded-wait tool), so a
   judge can wait on a real suite without inventing a loop.

Until that lands, the rule in `AGENTS.md` and `scripts/wait-for-command.sh` are
the crier-side answer, and they only bind judges that read this repository's
instructions.
