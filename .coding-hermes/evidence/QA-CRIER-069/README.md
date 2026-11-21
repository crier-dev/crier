# QA-CRIER-069 — chaos-corruption snapshot anchored to HOME instead of the repo

Project: crier. Fix surface: `~/.hermes/scripts/bunker-qa.sh` (shared host QA harness —
deliberately outside this git repo, so this directory carries the proof, not the code).

## What was wrong

`bunker-qa.sh` generates the remote battery script `qa-run.sh` through `build_remote_script()`
(an unquoted heredoc). Inside the generated script the two "state candidate" snapshots ran with
`.` = the runner's cwd, which is `$HOME` (the generated script only `cd`s into the clone inside
`qa_start_in_dir()`). The chaos-corruption cell then consumed those paths **relative to the repo**
(`cp "$SF" "$corr_dir/$SF"`). A snapshot taken at `$HOME` therefore selected surfaces that cannot be
opened from the repo, both damage controls SKIPped, and the cell still graded `FAIL`:

```
text/append-log state ./qa-evidence.jsonl (referenced by ./scripts/lib/heredoc-escape-lint-selftest.sh);
torn-line control: SKIP - could not copy the surface into the scratch dir;
malformed-line control: SKIP - could not copy the surface into the scratch dir
```

`./qa-evidence.jsonl` is the battery's own output file. Observed live on tick
`crier-qa-2026-11-21-11-47-53` (tested HEAD `f1ed7725`); the same run's snapshot also carried
`./qa-crier/.gitreins/qa-ledger.jsonl` and `./qa-up-prev/.gitreins/qa-ledger.jsonl` — leftover
clones under the agent's scratch base. Vacuous red inside the no-worst-case gate: it recurs every
tick and the project's real append-log resilience is never exercised.

## What changed (one generic patch, no project names)

1. `QAREPO="\$HOME/qa-\$PROJ"` is defined in the generated script, next to `PROJ=`.
2. Both state snapshots run in a subshell that `cd`s to `$QAREPO`, so emitted paths are repo-relative
   **and** openable by the copy step.
3. The chaos-corruption cell body runs in `( if cd "$QAREPO"; then … else cell chaos-corruption N/A
   "repository unavailable: $QAREPO"; fi )` — snapshot, late scan, `state_ref` grep and `cp` now share
   one cwd contract. `cell()` appends to `$EVID`, so wrapping it in a subshell loses no row.
4. Harness-owned paths are excluded from selection: `-not -name 'qa-evidence.jsonl'` and
   `-not -path './qa-logs-*/*'` on both the text snapshot and its late scan.
5. Both-SKIP now grades `SKIP` (was `FAIL`): `elif [ "$text_torn_status" = SKIP ] && [ "$text_mal_status" = SKIP ]; then text_overall=SKIP`.
   `FAIL` still means a control **executed** and the app silently accepted corrupted state.
6. The `*.db` branch's behaviour is unchanged; only its candidate scans inherit the repo anchor and
   the `qa-logs-*` exclusion.

Cross-board twin: `QA-HERMES-CANOPY-060` reports the same mechanism from the same heredoc; this patch
is project-name-free, so the canopy lane inherits the fix (host sha256 below) without a second edit.

## Gates (run by the foreman on the installed file, not just the candidate)

| gate | result |
|---|---|
| `bash -n ~/.hermes/scripts/bunker-qa.sh` | PASS |
| `shellcheck -S warning` (findings) | 12 before / 12 after — no new, none fixed |
| `shfmt` | not installed on this host — not run |
| `test_bunker_qa_chaos_snapshot_anchor.sh` (installed) | PASS ×3 (`rc=0`) |
| same test vs pre-fix backup | FAIL (`rc=1`) — RED proven, reproduces `./qa-evidence.jsonl` |
| rendered `qa-run.sh` inspection | `QAREPO` and `qa-evidence.jsonl` survive heredoc generation |

```
PASS: snapshot selects only repo state and both controls execute
PASS: vanished surface and zero controls grade SKIP with reason
PASS: executed failing control remains FAIL
```

```
FAIL: expected real repo surface and OK controls; got chaos-corruption	OK	text/append-log state ./qa-evidence.jsonl (referenced by ./qa-sample/app.sh); torn-line control: OK — control executed; malformed-line control: OK — control executed
```

## Hashes

```
bunker-qa.sh before  07723439d200281634248a2e6a2f82712c3af87f911f5e0d2fbc2e7881bde6e3
bunker-qa.sh after   ed55acafd9e715d9e8e1b2b30f662126a4b00b1b5a39dc01abe1f971f70ce75d
backup               ~/.hermes/scripts/bunker-qa.sh.bak-20261121-0824-pre-QA-CRIER-069
install              candidate built at bunker-qa.sh.new, gated, then atomic mv (never in-place)
```

## Not verified here

A full remote bunker battery and real application corruption recovery — that is the QA lane's job
(costs a remote agent). This row proves the selection/anchor/classification logic only.

Files in this directory: `bunker-qa-chaos-anchor.diff` (full `diff -u` backup→installed),
`test_bunker_qa_chaos_snapshot_anchor.sh` (the red-first regression test as installed).
