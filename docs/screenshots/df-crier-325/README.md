# DF-CRIER-325 — message-to-Tasks affordance

## Behavior

A task-bearing message says `Lifecycle lives in the Tasks panel.` and exposes a
native `button` named `Open in Tasks` only if the loaded Tasks panel has the same
`task_id`. Activation shares the existing `.task-link[data-thread]` handler,
sets the thread filter and moves keyboard focus to that task's panel control.
The transcript's state remains a send-time snapshot, explicitly labelled
`recorded at send time, not live`; an absent snapshot still says
`task state: not exposed`. No lifecycle data is copied from the Tasks panel.

Missing tasks (including a failed task-list read) show
`Task not in the loaded task list.` with no operable control. A different task
in the same thread does not count as a match. Both read completion orders work.
An in-flight send temporarily disables navigation. An unchanged refresh retains
keyboard focus on the bubble control.

## Browser evidence

Run the focused fixture regression:

    NODE_PATH=/tmp/crier-browser/node_modules node scripts/chat-task-link.cjs

Run the integrated positive control and fixture regressions against a fresh,
stock Go test server:

    NODE_PATH=/tmp/crier-browser/node_modules go test ./cmd/server -run '^TestChatClientTaskMessageLinkBrowser$' -count=1 -timeout 120s -v

The integrated control creates actual agents, a session and a task via the API,
then activates the bubble button and checks the selected thread and focused Tasks
row. It consumes the actual transcript `task.task_id` wire field, not the stored
record's different `task.id` spelling. The follow-up Chromium checks explicitly
intercept API responses to exercise absent tasks, different IDs in the same
thread, failed reads and either read completion order. These fixture checks do
not claim a task runtime executed. No production records are used.

`results.json` records the fixture run; PNGs show the populated task and the
absent-task wording, including cropped bubble details.

## Pre-change RED control

Historical source: `7c476f2e12d0ae72e7613b38b884560e02a40023:cmd/server/chat.html`.

    git show 7c476f2e12d0ae72e7613b38b884560e02a40023:cmd/server/chat.html > /tmp/df-crier-325-pre-change.html
    NODE_PATH=/tmp/crier-browser/node_modules CHAT_TEST_HTML=/tmp/df-crier-325-pre-change.html node scripts/chat-task-link.cjs

This exits 1 with:

    AssertionError [ERR_ASSERTION]: task-bearing bubble must expose an Open in Tasks button
    0 !== 1

The same test passes against the changed page. The historical HTML is served
only to the test browser; the worktree file is never reverted or overwritten.

## Build and test gates

- `go build -p 4 ./...`: exit 0.
- `go vet -p 4 ./...`: exit 0.
- `NODE_PATH=/tmp/crier-browser/node_modules go test -p 4 ./cmd/server -count=1 -timeout 600s`: PASS (205.844s).
- `NODE_PATH=/tmp/crier-browser/node_modules go test -p 4 ./... -count=1 -short -parallel 4`: PASS.
- `node --check scripts/chat-task-link.cjs`: exit 0.
- `gitreins guard --full`: Tier 1 PASS (full, whole tree); secrets clean, build/vet OK, tests passed.

## Broader harness limitation (outside this task)

An extra run of `scripts/chat-ui-browser.cjs` stops at its existing
`assertActionGates` comparison: the Claim button title contains only the reason
sentence, while `#composer-why.innerText` also contains the inline `Set acting ID`
button label. The exact mismatch is independently reproduced using the
historical HTML above. This task does not change the composer reason row or fix
that unrelated assertion. The focused Go/Chromium regression passes independently.
