# DF-CRIER-382 — readable desktop capability matrix

## Layout decision

At viewport widths >=1024px, move the existing matrix panel immediately after
`#transcript` in the main conversation column, using that column's full width.
Below 1024px, restore its original position in the context rail. There is only
one grid and one renderer. This uses the empty conversation space without an
extra expand action and without putting the matrix below the entire, much
longer inspector. Matrix-specific alignment prevents empty panels stretching
into blank grid bands. Composer markup/styles, reason row, posture, delivery
outcomes, APIs and declaration rendering are unchanged.

## Browser evidence

The Go-launched browser test registers six agents through the real `/agents`
API, creates a real six-member channel, and reads the embedded `/chat` page.
Every viewport retains `matrix-test-agent-1` through `matrix-test-agent-6`.
The declaration-only `ask` legend and `Not declared` cell labels are asserted.
Column counts are measured from actual header bounds and compared with the
notice after resizing and scrolling, not guessed from viewport widths.

| Viewport | Page overflow | Body overflow | Matrix viewport | Matrix overflow | Fully visible |
| --- | --- | --- | --- | --- | --- |
| 390px | 0px | 0px | 366px | 214px | 3/6 |
| 768px | 0px | 0px | 361px | 219px | 3/6 |
| 1280px | 0px | 0px | 700px | 0px | 6/6 |

Actual agent-column widths are 80px at 390/768 and 100px at 1280. The
1280px notice is `All 6 agent columns shown (this realm).` The small-screen
notice remains `Showing 3 of 6 agent columns fully — scroll the matrix to view
all declarations. All agents in this realm are included.`

Additional coverage: fresh desktop navigation; same-panel reparenting across
1023/1024; keyboard ArrowLeft and reaching the last agent; all-undeclared
headers; fixture registry refreshes with zero and one agent at all three
required widths. Those compact panels measure 93/93/112px high for zero agents
and 148.5/148.5/167.5px for one agent, with zero page/body/matrix overflow.
Only those compact/undeclared registry reads are intercepted; the six-agent
acceptance uses real API writes.

Boundary residual: the existing shell switches from two to three columns at
1101px. The transcript-width matrix there has 59px internal overflow and
truthfully reports 5/6 fully visible. It fits all six again at 1160px; 1024,
1100, 1280, 1440 and 1920 also fit all six. Page/body overflow remains zero.
This does not change the surrounding shell or squeeze readable column widths.

Screenshots:

- `chat-matrix-six-390.png`
- `chat-matrix-six-768.png`
- `chat-matrix-six-1280.png`
- `chat-matrix-six-detail.png` (desktop panel crop)
- `six-agent-measurements.json` (raw required-width and compact-state measurements)

## Pre-change rejection

With the final browser assertions and only `cmd/server/chat.html` temporarily
replaced by the exact blob from base commit
`b942d190bc7861549acbab6e0d4797bcc0d73565`, the targeted Go test exits 1:

    MATRIX MEASUREMENT {"width":1280,"pageOverflow":0,"bodyOverflow":0,"matrixWidth":251,"matrixOverflow":329,"columnWidths":[80,80,80,80,80,80],"fullyVisible":1,"panelHeight":348.5,"inRail":true,"notice":"Showing 1 of 6 agent columns fully — scroll the matrix to view all declarations. All agents in this realm are included."}
    AssertionError [ERR_ASSERTION]: desktop must show all six agent columns fully without scrolling
    1 !== 6

The fixed source was restored byte-identically in a `finally` block, then the
same test passed. Full negative-control log: `/tmp/df-crier-382-red-final.log`;
positive log: `/tmp/df-crier-382-green-final.log`.

## Reproduction

Playwright is installed outside the repository; browser is
`/usr/bin/google-chrome`. Set NODE_PATH to that installation's node_modules.

    NODE_PATH=/tmp/crier-browser/node_modules GOMAXPROCS=4 go test -p 4 ./cmd/server -run '^TestChatClientCapabilityMatrixAllAgentsBrowser$' -count=1 -v -timeout 180s

Set `CHAT_MATRIX_EVIDENCE` to an output directory to regenerate the screenshots
and JSON. A missing browser prerequisite is an explicit SKIP, never PASS.

The existing ten-agent hostile-content/realm-filtering matrix battery also
passes on a freshly built server (explicit matrix-only mode):

    GOMAXPROCS=4 go build -p 4 -o /tmp/df-crier-382-server ./cmd/server
    CRIER_TEST_BINARY=/tmp/df-crier-382-server NODE_PATH=/tmp/crier-browser/node_modules CHAT_MATRIX_ONLY=1 CHAT_EVIDENCE=/tmp/df-crier-382-hostile node scripts/chat-ui-browser.cjs

Log: `/tmp/df-crier-382-hostile.log`. No production server was changed.

## Final gates

All exited 0 on the fixed tree with GOMAXPROCS=4 and Go build concurrency `-p 4`:

- `go build ./...`
- `go vet ./...`
- `go test ./cmd/server -count=1 -timeout 600s` (browser dependency supplied via NODE_PATH)
- `gitreins guard --full`: secrets clean, go_build ok, go_lint ok, go_tests passed

Server-suite log: `/tmp/df-crier-382-server-tests-final.log`.
Guard log: `/tmp/df-crier-382-guard.log`.
