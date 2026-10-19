# DF-CRIER-386 — repair stale browser reason assertions

## Verdict and exact baseline observations

The harness was wrong, not the product. Baseline `origin/main` and this worker's
starting HEAD were both `91c6e8a0c9f261c38aa2277cce9565841f42f614`.
`cmd/server/chat.html` is unchanged by this task: reason vocabulary, gate logic,
authority policy, recovery controls and delivery paths are byte-identical.

All six exact observations are retained in
`DF-CRIER-386-task-action-gates.json`. `title` is the actual task button attribute,
`panel` is the actual `#composer-why.innerText`, and `reason` is the dedicated
`#composer-reason.innerText`. JSON `\n` escapes preserve the observed line breaks.
Both Claim and Complete produced the following identical pairs:

| Precondition | Button title (verbatim) | Whole panel innerText (JSON-escaped newlines) |
| --- | --- | --- |
| identity | `Set an acting agent ID (Connection & identity, left) to enable sending.` | `Set an acting agent ID (Connection & identity, left) to enable sending.\nSet acting ID` |
| session | `No channel open — pick or create one.` | `No channel open — pick or create one.\nPick a channel\nCreate channel` |
| closed | `This conversation is closed; sending is unavailable. Pick or create an open channel.` | `This conversation is closed; sending is unavailable. Pick or create an open channel.\nPick a channel\nCreate channel` |

Every title already equals the dedicated reason exactly. DF-CRIER-367 added
inline recovery buttons inside the panel (see `DF-CRIER-367-composer-state.md`).
The old assertion incorrectly required those button labels to be in the title.
It was a stale DOM boundary, not a hard-coded sentence in that assertion.

The fixed harness reads the task button and dedicated reason independently from
the rendered DOM in one browser evaluation. It requires a disabled task button,
a visible nonempty reason, and exact title/reason equality for every combination.
It does not call `composerReason()` to manufacture an expected value, copy a title
into the reason, remove recovery controls or change product text. State injection
and observation happen atomically; `finally` restores the fixture even on failure.
Every run writes `task-action-gates.json`, including partial observations on failure.

## RED control and diagnostic capture

The original unmodified harness and freshly built baseline binary exited 1:

    go build -p 4 -o /tmp/df-crier-386-server ./cmd/server
    NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-386-server CHAT_EVIDENCE=/tmp/df-crier-386-baseline node scripts/chat-ui-browser.cjs

`/tmp/df-crier-386-baseline.log` records the Claim/identity failure at the original
line 213: actual is the reason sentence, expected additionally contains
`\nSet acting ID`. It passed the posture/matrix checks before that failure.

To collect the remaining pairs, a temporary copy of the historical harness
replaced ONLY this failing equality assertion with JSONL logging. Its two helper
modules were copied alongside it. The binary and embedded HTML remained baseline;
the diagnostic run is NOT a full-suite PASS. Source provenance was pinned with
`git show 91c6e8a0c9f261c38aa2277cce9565841f42f614:scripts/chat-ui-browser.cjs`.
Raw capture: `/tmp/df-crier-386-capture-evidence/baseline-pairs.jsonl`;
log: `/tmp/df-crier-386-capture.log`; preparation: `/tmp/df-crier-386-capture.py`.
The committed JSON pairs were programmatically compared with all six raw rows.

Continuing also exposed a second stale harness expectation, previously hidden by
the first: its signature-required test tried to fill the now-correctly-disabled
textarea. The diagnostic run exited 1 with `locator.fill: Timeout 30000ms exceeded`
and `element is not enabled`, at the `Unsigned enforced browser send` step.
The fixed harness asserts disabled textarea/Send plus a visible matching reason.
A separate, explicitly labeled Node HTTP POST still verifies the real server's
401 `SESSION_SENDER_SIGNATURE_REQUIRED` and an unchanged empty transcript. It does
not force-enable the UI or describe the HTTP probe as a browser click.

## GREEN controls and anti-tautology proof

The fixed harness ran against that SAME baseline binary twice, with isolated
scratch servers/storage and full-suite mode (no `CHAT_MATRIX_ONLY`):

    NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-386-server CHAT_EVIDENCE=/tmp/df-crier-386-green-1 node scripts/chat-ui-browser.cjs
    NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-386-server CHAT_EVIDENCE=/tmp/df-crier-386-green-2 node scripts/chat-ui-browser.cjs

Both exited 0, with all six gate observations, real open → claimed → done
transitions, exact completion evidence readback, 390/768/1280 viewport checks and
zero page errors. The corresponding `/tmp/df-crier-386-green-{1,2}.log` files end
in the full-suite PASS marker. This is RED old harness / GREEN fixed harness on
unchanged pre-change product HTML and binary, not a product fix disguised as a
test update.

    NODE_PATH=/tmp/crier-browser/node_modules CRIER_TEST_BINARY=/tmp/df-crier-386-server node scripts/chat-ui-browser-selftest.cjs

The extended selftest exited 0. It first requires a clean full-suite exit 0 and
exact coverage of the six unique action/precondition pairs. Temporary harness
copies then change only the real DOM button title to
`TEST-ONLY mismatched task reason`, leaving the visible reason untouched:

- wrong-title-claim-identity: exit 1, Claim/identity title/reason assertion.
- wrong-title-claim-session: exit 1, Claim/session title/reason assertion.
- wrong-title-claim-closed: exit 1, Claim/closed title/reason assertion.
- wrong-title-complete-identity: exit 1, Complete/identity title/reason assertion.
- wrong-title-complete-session: exit 1, Complete/session title/reason assertion.
- wrong-title-complete-closed: exit 1, Complete/closed title/reason assertion.
- Equal-but-empty title AND reason: exit 1 at the nonempty-visible-reason assertion.

Existing controls remain: missing completion evidence exits 1 on the real 409
`TASK_EVIDENCE_REQUIRED`, wrong revision exits 1, and unavailable browser exits 77
without claiming browser assertions. No mutation edits the worktree or product.
Selftest log: `/tmp/df-crier-386-selftest.log`; per-control stdout/stderr and machine
results: `/var/tmp/hermes-scratch/crier-browser-selftest-CZJJMS/`.

## Verification

- `GOFLAGS=-p=4 go build ./...`: exit 0.
- `GOFLAGS=-p=4 go vet ./...`: exit 0.
- `NODE_PATH=/tmp/crier-browser/node_modules GOFLAGS=-p=4 go test ./cmd/server -count=1 -parallel 4 -timeout 900s`: exit 0, 101.352s; `/tmp/df-crier-386-server-tests.log`.
- `GOFLAGS=-p=4 go test ./... -count=1 -short -parallel 4 -timeout 900s`: exit 0; `/tmp/df-crier-386-short.log`.
- Staged `GOFLAGS=-p=4 /home/kara/.local/bin/gitreins guard --full`: exit 0,
  full whole-tree PASS, four guards, zero failed/skipped, correct worktree attribution;
  `/tmp/df-crier-386-guard.log` and `.gitreins/logs/guard-20261019T180735.935605Z.log`.
- Both full browser runs and the expanded selftest: exit 0 as above.
- `node --check` on both harness scripts: exit 0.
- `/tmp/df-crier-386-verify.py`: captured/committed pair parity, six-gate coverage,
  full-suite reports, exact mutation assertion failures, and unchanged product/board.

No push, merge, deployment, dependency install, product change or board edit is
part of this task.
