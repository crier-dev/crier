# QA-CRIER-32 — heredoc-escaping lint for the bunker-qa.sh generator

- **Task:** `QA-CRIER-32` — heredoc-escaping lint for the bunker-qa.sh generator
- **Tick / date:** tick `379` — `2026-09-21` (UTC-05)
- **Operation:** added `scripts/lib/heredoc-escape-lint.sh` + selftest, wired
  `make heredoc-lint` / `make heredoc-lint-selftest`, and — en route — proved the
  class this lint guards is LIVE in the current generator (3 unescaped backticks
  landed with QA-CRIER-19's ui-probe arm today; two of them run `pkill` on this
  host at every generation run).
- **Targets:**
  - repo(s): `crier` (this commit — shell + Makefile + evidence only)
  - read-only: `~/.hermes/scripts/bunker-qa.sh` (fleet-shared, untracked; NOT
    modified — AC-8 holds)
- **⚠ LIVE FINDING (surface to the QA foreman / QA-CRIER-19 owner):** the
  brief's AC-1 premise ("the current generator MUST pass") was true when the
  brief was written but the QA-CRIER-19 ui-probe arm landed in the generator
  today (2026-09-21) carrying unescaped backticks at generator lines 857, 982,
  984. Inside the unquoted heredoc they EXECUTE AT GENERATION TIME on this
  host — PATH-shim proof below shows `curl -sSL` (no URL: the recurring
  `curl: (2) no URL specified` stderr), `pkill -f 'npm run dev'` and
  `pkill -f 'node .*vite'` all fire on every `run`/`launch`/`__gen-remote`.
  `make heredoc-lint` exits 1 naming those lines until the generator escapes
  them (`\`` → `` \` ``). The lint is per-spec; the generator is red.

## Lint design (what it flags / allows)

`scripts/lib/heredoc-escape-lint.sh [FILE|-]` — default target is the live
`~/.hermes/scripts/bunker-qa.sh`. It locates the ONE unquoted heredoc
(`^[[:space:]]*cat[[:space:]]*<<EOF[[:space:]]*$` … `^EOF$`, must be preceded
by `build_remote_script`; quoted/`<<-`/`<<'PY'` forms never match) and scans
the body lines with absolute line numbers.

Flags (fail-closed; heuristic documented in the header):

- **R1 backtick** — any unescaped backtick in the body (lookbehind keeps the
  14 sanctioned `` \` `` forms green). Heredoc lines are data, not shell: a
  backtick is a host-side command substitution EVEN ON A `#` COMMENT LINE —
  that is exactly how the curl-in-a-comment incident fired.
- **R1b double-escaped backtick** `\\`` — bakes a literal backslash plus a
  live backtick.
- **R2 double-escaped dollar** `\\$` — the incident-1 shape (`\$\(curl\)`):
  escapes the BACKSLASH, not the expansion; generator bakes `\` + a LIVE
  `$(…)`/`${…}` into the remote script.
- **R3a bare expansion** — unescaped `$WORD` / `$(` / `${` outside the
  generation-time bake allowlist (`$proj $agent $install_cmd $ci_cmd
  $native_cmd $ACT_URL $DETECT_TAG_COUNT $DETECT_PREV_TAG
  $DETECT_PREV_VERSION $DETECT_PKG_NAME $DETECT_PKG_ECOSYSTEM`). A single
  preceding `\` (correct escape) skips; per-match consumption is by OFFSET
  (`${var#pat}` only strips at string head — the first draft spun forever on
  mid-line tokens; fixed and noted in-code).
- **R3b bare specials** — `$? $! $* $@ $# $$ $- $0-9` anywhere: bakes the
  GENERATOR's value (or aborts under `set -u`); never legitimate.
- Comment-line-only allowances (first non-blank char `#`; the same bare form
  in CODE is still rejected): `$PATH` (log-prose quoting; the code's
  `[$]PATH` idiom is naturally outside the scan) and `$(ulimit -v)`
  (QA-WARPFS-11 comment; pure command, retained deliberately).

Exit codes: `0` clean (PASS names the scanned region line range) · `1`
violations (each finding on stderr with its line number) or heredoc region
unlocatable · `2` generator path missing/unreadable — fail closed naming the
path, never a silent skip. Selftest neuters the single `_reject`
choke-point (`NEUTER-MARK[reject]`) via cmp-guarded sed — the same pattern as
`check-gofmt.sh`.

## AC-1 — clean-generator pass

As literally written this is UNSATISFIABLE today: the live generator carries
3 real backtick defects (see LIVE FINDING). Verified instead on a
backtick-scrubbed full-scale copy (selftest check 1b) — the accept path on
real-scale input:

- **Command run:** `make heredoc-lint-selftest` (check 1b inside)
- **Observed output:**
  ```text
  PASS: a backtick-scrubbed full-scale generator copy is ACCEPTED (rc=0), scan named:
    heredoc-escape-lint.sh: PASS — 0 violation(s); scanned build_remote_script heredoc of
    /tmp/heredoc-escape-lint-selftest.BvERr7/live-scrubbed.sh (cat<<EOF line 430, EOF line 1169, body lines 431-1168)
  ```
- **Current live run (`make heredoc-lint`), showing the 3 live findings:**
  ```text
  heredoc-escape-lint.sh: FAIL  backtick  line 857: … a bare `curl -sSL` silently saved that body as the
  heredoc-escape-lint.sh: FAIL  backtick  line 982: … the root arm's bare `pkill -f 'npm run dev'` LEAKS
  heredoc-escape-lint.sh: FAIL  backtick  line 984: … `pkill -f 'node .*vite'` safety net is worse …
  heredoc-escape-lint.sh: FAIL — 3 violation(s) in build_remote_script heredoc (body lines 431-1168)
  ```

## AC-2 — selftest incl. neuter proof

- **Command run:** `make heredoc-lint-selftest`
- **Observed output:**
  ```text
  PASS: the live generator is scanned end-to-end, region and verdict reported (… FAIL)
  PASS: a backtick-scrubbed full-scale generator copy is ACCEPTED (rc=0) …
  PASS: the seeded double-class fixture (backticked command + double-escaped \$(curl) + bare $T) is rejected with line numbers (rc=1)
  PASS: an unescaped backtick in a comment line is rejected (rc=1) — the 09-21 incident shape
  PASS: a double-escaped \$( (the incident-1 shape) is rejected (rc=1)
  PASS: a bare $UNBOUND_VAR (the set -u killer) is rejected (rc=1)
  PASS: bare $? and ${…} are rejected (rc=1)
  PASS: a missing generator path is exit 2 naming the path (rc=2)
  PASS: NEUTER PROOF: the neutered copy differs (cmp) and ACCEPTS the same fixture the real lint rejects (rc=0) …
  heredoc-escape-lint-selftest.sh selftest: 9/9 checks behaved
  ```

## AC-3 — seeded-fixture rejections carry line numbers

- **Command run:** seeded bad body
  `BX_TAG=`hostname` && T=\\$(curl -fsSL https://x | head -1) && echo $T`
- **Observed output:**
  ```text
  heredoc-escape-lint.sh: FAIL  backtick      line 11: unescaped backtick in heredoc body — command substitution executes ON THE GENERATION HOST …
  heredoc-escape-lint.sh: FAIL  bare-$-token  line 11: bare \$(…)/\${…} expansion in heredoc body — expands at generation time (silent corruption) or aborts under set -u …
  ```
  (the seeded `\\$(curl` also raises the R2 double-escape finding when the
  backslash doubling is exact; the `$T` bare-var and backtick findings above
  are the isolated-class proof, plus separate single-class fixtures for
  backtick / `\\$( ` / `$UNBOUND_VAR` / `$?`+`${…}` — all rejected, rc=1.)

## AC-4 — missing generator is exit 2 naming the path

Covered by selftest check 4 (`PASS: a missing generator path is exit 2 naming
the path (rc=2)`); direct run `bash scripts/lib/heredoc-escape-lint.sh
/tmp/no-such-generator.sh` → rc=2, stderr
`heredoc-escape-lint.sh: generator not readable: /tmp/no-such-generator.sh`.

## AC-5 — bash -n clean

- **Command run:** `bash scripts/check-shell-yaml.sh scripts/lib/heredoc-escape-lint.sh scripts/lib/heredoc-escape-lint-selftest.sh`
- **Observed output:**
  ```text
  PASS  shell     scripts/lib/heredoc-escape-lint.sh
  PASS  shell     scripts/lib/heredoc-escape-lint-selftest.sh
  check-shell-yaml: scope: 2 shell file(s), 0 workflow file(s) verified
  check-shell-yaml: PASS — 2 file(s) checked, 0 rejected
  ```

## AC-6 — make shell-yaml-check still passes

- **Command run:** `make shell-yaml-check && make make-docker-check && make -n heredoc-lint heredoc-lint-selftest`
- **Observed output (tails):**
  ```text
  check-shell-yaml: scope: 27 shell file(s), 3 workflow file(s) verified
  check-shell-yaml: PASS — 30 file(s) checked, 0 rejected
  check-make-docker: PASS — 11 file(s) checked, 0 rejected
  bash scripts/lib/heredoc-escape-lint.sh
  bash scripts/lib/heredoc-escape-lint-selftest.sh
  ```

## Make-target wiring

`Makefile`: `heredoc-lint` + `heredoc-lint-selftest` targets (pattern matches
`shell-yaml-check`/`gofmt-check`), both added to `.PHONY`, comment block
documents scope + the deliberate NOT-wiring into `shell-yaml-check` (that arm
reads TRACKED repo scripts; this lints an UNTRACKED fleet-shared file).

## LIVE FINDING evidence — host-side execution proof (PATH shims)

- **Command run:** shim `curl`/`pkill`/`pgrep` on PATH, then
  `bash ~/.hermes/scripts/bunker-qa.sh __gen-remote shim-probe true true true`
- **Observed output:**
  ```text
  generation rc=0
  === shim calls during ONE generation run:
  SHIM-CALLED: curl -sSL
  SHIM-CALLED: pkill -f npm run dev
  SHIM-CALLED: pkill -f node .*vite
  ```
  Unshimmed, the same run prints `curl: (2) no URL specified` on stderr while
  exiting 0 — the silent-corruption shape this lint exists to catch before
  shipping. Fix for the generator owner: escape the three backticks as
  `` \` `` (lines 857/982/984) or drop them, per the file's own convention
  comment; `make heredoc-lint` then goes green with zero lint changes.

## AC-8 — generator untouched

`~/.hermes/scripts/bunker-qa.sh` was only ever read (lint/selftest/proofs);
no write of any kind was issued against it in this task.
