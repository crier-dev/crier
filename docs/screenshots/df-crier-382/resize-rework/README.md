# DF-CRIER-382 — resize notice rework

## Change

`placeCapabilityMatrix()` now calls `updateCapabilityMatrixView()` through
`requestAnimationFrame`, after moving the same panel. The existing
`ResizeObserver(updateCapabilityMatrixView)` still observes `#capability-matrix`
for element size changes, including rail resizes that do not cross a media-query
breakpoint. Copy is still chosen from current `scrollWidth > clientWidth`, with
fully visible columns counted from current header bounds. No layout breakpoints,
column widths, composer, API, board or realm logic changed.

## Same-page Chromium sequence

Six agents and a six-member channel are created through the real APIs. The page
starts at 1280px; there is no reload during the following sequence. Each snapshot
reads geometry, column bounds, panel parent and notice together. Assertions do
not wait for the desired sentence before taking these snapshots.

| Width | Matrix overflow | Fully visible | Notice |
| --- | --- | --- | --- |
| 1280 | 0px | 6/6 | All 6 agent columns shown (this realm). |
| 390 | 214px | 3/6 | Showing 3 of 6 agent columns fully — scroll the matrix to view all declarations. All agents in this realm are included. |
| 420 | 184px | 3/6 | Showing 3 of 6 agent columns fully — scroll the matrix to view all declarations. All agents in this realm are included. |
| 390 | 214px | 3/6 | Showing 3 of 6 agent columns fully — scroll the matrix to view all declarations. All agents in this realm are included. |
| 1280 | 0px | 6/6 | All 6 agent columns shown (this realm). |
| 768 | 219px | 3/6 | Showing 3 of 6 agent columns fully — scroll the matrix to view all declarations. All agents in this realm are included. |
| 1280 | 0px | 6/6 | All 6 agent columns shown (this realm). |

Page and body overflow are 0px in every snapshot. Below 1024px the panel is in
the rail; at 1280px it is directly after the transcript. All six agent headers
remain rendered throughout. The original breakpoint, keyboard-scroll,
all-undeclared and zero/one-agent checks also run. The compact fixtures now
explicitly clear capabilities rather than relying on unordered registry results
to put the undeclared agent first.

The element-only observer check holds the viewport at 1280px and temporarily
limits the matrix box to 390px: overflow 190px, 3/6 fully visible, matching
`Showing 3 of 6 ...` text. This check triggers no viewport or matchMedia change.
The temporary inline width is removed before further tests.

## Historical negative control — precise limitation

The exact `cmd/server/chat.html` blob from `92cf212` is byte-identical to the
rework branch's starting HTML (`09ce4ef`, which rebased the first change onto
newer A2A work). The control substitutes that historical HTML only, keeps the
new browser regression, rebuilds through `go test`, then restores the fixed HTML
in a Python `finally` block with byte-equality verification.

Important: the reported stale-label failure did NOT reproduce in this Chromium
run. Historical HTML already contained a ResizeObserver, and the same-page
1280→390→420→390→1280 geometry/copy assertions passed on it. Do not describe this
as a reproduced `All 6` label with narrow overflow.

The independent scheduling regression DOES fail on `92cf212`:

    MATRIX PLACEMENT TIMING {"synchronousReads":1,"settledReads":1}
    AssertionError [ERR_ASSERTION]: panel relocation must defer geometry reads until the animation frame
    1 !== 0

Fixed output:

    MATRIX PLACEMENT TIMING {"synchronousReads":0,"settledReads":1}

Thus the negative control proves the requested deferred-measurement contract,
not reproduction of the coordinator's stale-label observation. Red log:
`/tmp/df-crier-382-rework-red.log`. Green log:
`/tmp/df-crier-382-rework-green.log`. Three additional independent executions
passed (`-count=3`, `/tmp/df-crier-382-rework-repeat.log`).

## Reproduce

    NODE_PATH=/tmp/crier-browser/node_modules GOMAXPROCS=4 go test -p 4 ./cmd/server -run '^TestChatClientCapabilityMatrixAllAgentsBrowser$' -count=1 -v -timeout 180s

Set `CHAT_MATRIX_EVIDENCE` to an output directory to regenerate screenshots and
`six-agent-measurements.json`. Missing Playwright/Chromium remains an explicit
SKIP, not a claimed PASS. These are scratch, in-process server tests; no running
production service was replaced.

## Verified gates

All exited 0 on the fixed tree, with Go concurrency limited to four:

- `go build -p 4 ./...`
- `go vet -p 4 ./...`
- `go test -p 4 ./... -count=1 -short -parallel 4 -timeout 600s`
  with Playwright supplied through NODE_PATH (server package 99.595s).
- `gitreins guard --full`: whole-tree PASS, secrets clean, go_build/go_lint ok,
  go_tests passed (not a short-circuited test claim).
- Browser regression independently passed three consecutive executions.
- `node --check scripts/chat-matrix-all-agents.cjs` and `git diff --check`.

Suite log: `/tmp/df-crier-382-rework-suite.log`.
Guard log: `/tmp/df-crier-382-rework-guard.log`.
