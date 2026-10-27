# DF-CRIER-365 — browser harness green and rejection proof

## Run

Run from the repository root using Node, not Python. This extends the existing
[CR-CHAT-038 reproduction recipe](CR-CHAT-038-responsive-ui.md#reproduce):

```sh
go build -p 4 -o /tmp/df-crier-365-server ./cmd/server
# Only if the existing external harness dependency is absent:
npm install --prefix /tmp/crier-browser --no-audit --no-fund playwright
NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-365-server node scripts/chat-ui-browser.cjs
```

For the repeatable clean + deliberately broken + missing-browser controls:

```sh
NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-365-server node scripts/chat-ui-browser-selftest.cjs
```

The selftest runs the full harness once, then runs temporary copies with (1) an
empty completion evidence payload and (2) a wrong DOM revision expectation. It
requires rc 1 and a diagnostic naming the failing assertion for each copy. It
also requires rc 77 and a named skip reason for an unavailable browser. Mutation
anchors must be unique; a stale anchor fails the selftest. The selftest never
edits the source tree or the server, and ignores `CHAT_MATRIX_ONLY` so a partial
suite cannot masquerade as its clean control. Each child has a 180-second ceiling;
a killed/timed-out child fails rather than counts as a regression rejection.

`CHROME_BIN` selects an executable Chromium (default `/usr/bin/google-chrome`).
`CHAT_EVIDENCE` optionally selects the full harness's artifact directory; without
it each invocation creates a unique temporary evidence directory. The selftest
always uses its own unique directory and prints each child's stdout/stderr path.
Keep those artifacts with the run being reviewed, not a previous run's JSON.

Exit contract:

- 0: the selected scope passed. Full runs explicitly print `PASS: full browser
  suite`; `CHAT_MATRIX_ONLY=1` explicitly prints `MATRIX ONLY` and names omitted
  task/full-suite assertions. The selftest exits 0 only after both rejections and
  the missing-browser control also behave as expected.
- 1: assertion, server, browser-launch crash, or selftest failure. A missing server
  binary is an error, not a browser skip. Stderr names the failing operation.
- 77: Playwright unavailable, or `CHROME_BIN` missing/not executable. The reason
  and exit code are printed; no browser assertions ran. The selftest propagates
  77 and claims no regression proof when its clean control skips. A launch crash
  after executable preflight is NOT reclassified as a skip.

## Baseline actually measured

Source baseline: `39bc1bb0d2ed6f9211a0378cc2812fa7d9dbf5fc`, branch
`wt/DF-CRIER-365`. No server changes. Interpreter/tool versions in this run:
Node v22.22.3, Playwright 1.64.1, Google Chrome 152.0.7977.82.

Command run before changing the harness:

```sh
NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-365-server CHAT_EVIDENCE=/tmp/df-crier-365-baseline node scripts/chat-ui-browser.cjs
```

rc 1; stderr began exactly:

```text
AssertionError [ERR_ASSERTION]: Expected values to be strictly equal:

409 !== 201

    at run (/home/kara/worktrees/crier-DF-CRIER-365/scripts/chat-ui-browser.cjs:198:41)
```

Full original stderr: `/tmp/df-crier-365-baseline.stderr`; stdout:
`/tmp/df-crier-365-baseline.stdout`. The original diagnostic did not name the
response body. Inspection of the actual baseline shows both claim and complete
used `ui-test-operator`: this revision did NOT complete as a different owner.
It did send no evidence. The repaired harness reads and asserts the real 409
`TASK_EVIDENCE_REQUIRED` body and unchanged claimed task as its negative control.

Two additional stale expectations surfaced after that first failure was removed:

- Missing recipient diagnostics now display `no such agent (465)` plus
  `AGENT_NOT_FOUND`, not literal `refused`. The API outcome is still asserted to
  equal `refused`. Actual failure: `/tmp/df-crier-365-green3.stderr`.
- Participant error copy now includes the HTTP/code prefix. Actual value was
  `HTTP 403: TEST_REFUSAL: Exact participant refusal from test server.`, not the
  unprefixed message. Actual failure: `/tmp/df-crier-365-green4.stderr`.

The harness now asserts those exact current diagnostics; it does not suppress or
ignore them. No task ownership/evidence rule, embedded UI, board, or control-plane
record was modified to make the run green.

## What the green verifies

The harness switches browser attribution to the registered recipient, reselects
its session, clicks Claim, and verifies the request actor, HTTP 201, owner and
claimed state through GET. It clicks the stock Complete button and asserts the
real HTTP 409 `TASK_EVIDENCE_REQUIRED`, rendered refusal and unchanged task.

The UI has no evidence-entry control. Successful completion is therefore an
EXPLICIT BROWSER FETCH, not a claim that the Complete button works end to end:
`POST /sessions/:session/tasks/:task/complete` with the same claiming agent and a
nonempty payload. The evidence is a real `task-evidence.json` file containing
already-measured browser observations and a real claimed-task screenshot, with
file URLs pointing to both. Completion asserts 201/done/same owner, exact evidence
read back through GET, refreshed task DOM containing done, and no remaining task
action controls. The old bare-button refusal remains visible as a limitation.

The existing browser coverage remains: rendered server records, opt-in tab
attribution/reload/forget/storage denial, reply parent/thread readback, thread
isolation, addressing vs explicit task intent, missing-recipient refusal, draft
retention on faults, stale-read isolation, hostile markup escaping, search empty
state, session creation and initial members, participant admission, zero-recipient
confirmation and follow-up delivery, and layout/control/transcript reachability
at 390/768/1280 pixels. Browser page-error capture must remain empty.

Stdout distinguishes `BROWSER VERIFIED`, `BROWSER FETCH (not Complete form)` and
`API VERIFIED (not a browser action)`, prints the actual scratch URLs and actions,
and emits a JSON report with API requests/statuses, assertion groups, task
transitions, evidence, viewport measurements and explicit limitations.

## Recorded green and seeded regression runs

Standalone clean run: rc 0; stdout `/tmp/df-crier-365-green5.stdout`, stderr
`/tmp/df-crier-365-green5.stderr`; evidence `/tmp/df-crier-365-green5/`.

Repeatable selftest: rc 0; stdout `/tmp/df-crier-365-selftest.stdout`, stderr
`/tmp/df-crier-365-selftest.stderr`; child evidence root:
`/var/tmp/hermes-scratch/crier-browser-selftest-RoJsBz/`.

- `clean`: rc 0, full browser suite, actual completion evidence and screenshot.
- `missing-evidence`: rc 1. Temporary copy changed only the successful browser
  fetch's payload to `{}`. Real server returned 409. Stderr named:
  `browser fetch POST /sessions/3f2d1608598d16a1ba01ea29/tasks/bdb6e389dc3303327de55691/complete:`
  followed by `TASK_EVIDENCE_REQUIRED` and `409 !== 201`.
- `wrong-revision`: rc 1. Temporary copy expected `SEEDED_WRONG_REVISION` instead
  of the actual DOM revision. Stderr named `/chat UI revision must be CR-CHAT-038`
  and displayed actual `CR-CHAT-038` vs expected `SEEDED_WRONG_REVISION`.
- `browser-unavailable`: rc 77, `SKIP: Chromium executable unavailable at .../no-browser
  (ENOENT); set CHROME_BIN to an executable browser; exit code 77 (no browser assertions ran)`.

Each child's complete stdout/stderr is named under that root. Both deliberate
mutations are confined to temporary copies; neither is in the worktree or commit.

## Gates and limitations

Passed: `go build -p 4 ./...`, `go vet -p 4 ./...`, `node --check` on both harness
scripts, the harness and selftest above, and `make shell-yaml-check` (55 shell/YAML
files checked, actionlint v1.7.12; it does NOT validate JavaScript). Also passed:

```sh
go test -p 4 ./cmd/server ./internal/session -run 'TestTaskCompletionRequiresOwnerAndEvidence|TestTaskFailureByOwnerDoesNotRequireSuccessEvidence|TestChat' -count=1 -short -parallel 4
```

No Go files changed. The optional `gitreins guard --full` DID attempt the whole
Go suite and exited 1: SQLite registry tests fail on unchanged migration 017:
`initialize SQLite registry: SQLite migration 017: SQL logic error: near "~": syntax error (1)`.
The guard log is `/tmp/df-crier-365-guard.log` (full step summary also at
`.gitreins/logs/guard-20261019T134834.394121Z.log`). An isolated
`go test -p 4 ./internal/registry -run '^TestSQLiteStoreDurability$' -count=1 -short -parallel 4`
reproduced rc 1, logged at `/tmp/df-crier-365-baseline-sqlite.log`.
`git diff HEAD --name-only -- '*.go' '*.sql'` was empty: this is unchanged baseline
Go/SQL, outside this harness-only task. No guard bypass or config edit was used.
The default staged `gitreins guard` returned rc 0, explicitly DEGRADED PASS:
secrets clean, Go build/lint/tests skipped because no Go files staged (log:
`/tmp/df-crier-365-default-guard.log`). It is NOT evidence of whole-suite health.
Direct build/vet and the targeted uncached tests above are the scoped Go evidence.

Missing Playwright was also exercised with
`env -u NODE_PATH node scripts/chat-ui-browser.cjs`: rc 77 with the explicit
`SKIP: Playwright is not installed` reason and no browser assertions (stderr:
`/tmp/df-crier-365-no-playwright.stderr`).

Remaining limits are explicit: all state is scratch JSONL sessions/in-memory
registry and generated test records, not production data or a deployed-system
audit; guard is disabled in scratch config; no agent runtime executes the task;
evidence records harness observations only; successful completion uses browser
fetch because the UI lacks evidence entry; token-protected `/chat` is checked
with Node HTTP fetch (401 without token, 200 with token), not browser login;
capability-matrix registry shapes, malicious read bodies, transport errors,
storage denial, UI gating states and delayed reads are injected test fixtures,
not real upstream failures. This does not test PostgreSQL, restart durability,
production permissions, or every browser. Temporary evidence and scratch server
logs are retained; cleanup of old artifacts remains the operator's responsibility.

> SUPERSEDED 2026-10-27 (DF-CRIER-333 / DF-CRIER-335, commit `92b0ec0e`): the
> token-protected `/chat` limit above no longer holds. `scripts/chat-ui-browser.cjs`
> now also drives a REAL browser session — 401 with the DF-CRIER-444
> fronting-requirement page and no panel without a front; the live panel through a
> front that injects the header on the wire, whose OWN protected `GET /sessions`
> returned a token-created record; the token in neither page source nor
> localStorage/sessionStorage/cookies. The front in that arm is a harness stand-in:
> WHICH fronting arrangement ships is still the open DF-CRIER-333 owner ruling.
> Evidence: `docs/dogfood/screenshots-2026-10-27/README.md`.
