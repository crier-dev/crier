# DF-CRIER-460 — lifecycle authority evidence

## Scope

Client-only production change in `cmd/server/chat.html`. No server handler,
credential mechanism, token field, Bearer proxy, permission rule or OpenAPI
change. The session read selects Close versus Reopen. The existing `GET /status`
response supplies ACL posture:

- `management_surface: disabled`: ACL off; retain the existing acting-ID and
  signature prerequisites and unchanged `{state, sender}` lifecycle body.
- `enabled-unarmed` or `admin-token-gated`: ACL enabled. Neither a management
  read nor an acting agent ID establishes a bound principal's session-admin
  authority. Disable the applicable control and display that missing requirement
  beside it, linked with `aria-describedby`.
- Missing, future, pending or failed posture read: unknown, disabled, with reason.

The handler and button share `lifecycleReason()`. A racing 403 reuses
`managementFailureKind()` and the existing `management-read refused` styling
from DF-CRIER-459. Its full server error remains text content, not HTML, and the
page refreshes state without assuming a local transition.

## Reproducible historical RED, then GREEN

Run from the repository root:

```sh
git show e0db6a697b8c01d0ee6ede97948d1ca4a47bb292:cmd/server/chat.html > /tmp/df-crier-460-before.html
node scripts/chat-lifecycle-truth.cjs --html /tmp/df-crier-460-before.html
node scripts/chat-lifecycle-truth.cjs
```

Actual historical result: exit 1, `RESULT 4/24 passed; 20 failed`.
Actual fixed result: exit 0, `RESULT 24/24 passed; 0 failed`.

| Assertion | Historical page | Fixed page |
| --- | --- | --- |
| (a) Armed ACL disables the visible control | FAIL: `false !== true` | PASS |
| (a) Inline explanation names the bound principal, session admin and DF-CRIER-333 | FAIL: empty text | PASS |
| (a) Neither native disabled click nor direct handler can POST | FAIL: `1 !== 0` POSTs | PASS: zero POSTs |
| (b) Unarmed Close and Reopen remain enabled and retain body bytes | PASS for both directions | PASS for both directions |
| (c) Racing 403 is refused, not red, with verbatim cause | FAIL: class was `note error` | PASS: `note management-read refused` |

The armed matrix covers both `enabled-unarmed` and `admin-token-gated`, with
both open and closed sessions. Other checks cover unknown posture, failed and
pending refreshes, existing signature/actor/busy gates, unreadable session state,
500 and malformed-403 failures, and the shipped accessible-description links.

Requirement (b) is a preservation control: it already worked historically, so
claiming historical RED would be false. Its falsifiability was proven separately:
a scratch copy of the fixed page changed only
`body:JSON.stringify({state:next,sender:state.agent})` to
`body:JSON.stringify({state:next})`. Running the same `--html` harness against
that explicitly mutated copy exited 1 with `RESULT 22/24 passed; 2 failed`;
only the two body-preservation cases failed, showing the missing `sender`.
The historical page was not mutated.

Local transcripts:

- `/tmp/df-crier-460-red.log`
- `/tmp/df-crier-460-green.log`
- `/tmp/df-crier-460-mutation-red.log`

## Existing regression harnesses

```sh
node scripts/chat-refusal-truth.cjs
node scripts/chat-inflight-truth.cjs
```

Actual output: `RESULT 41/41 passed; 0 failed` and
`RESULT 16/16 passed; 0 failed`, respectively (both exit 0).

`TestChatLifecycleTruth` runs the selectable-file lifecycle harness from Go, so
it is part of the ordinary `cmd/server` test suite without a new dependency.

## Build, vet and targeted tests

```sh
go build -p 4 ./...
go vet -p 4 ./...
go test -p 4 ./cmd/server -count=1 -short -parallel 4 -run 'Test(Chat|StatusManagementSurface|ManagementPosture|OpenAPI)' -v
go test -p 4 ./internal/session -count=1 -short -parallel 4 -run '^TestSessionLifecycle' -v
cmp docs/openapi.yaml cmd/server/openapi.yaml
```

All commands exited 0. Build and vet emitted no diagnostics. Test output ended:

```text
PASS
ok  github.com/crier-dev/crier/cmd/server       2.255s
PASS
ok  github.com/crier-dev/crier/internal/session 3.646s
OPENAPI BYTE-IDENTICAL
```

The existing lifecycle tests verified attributed event persistence across
store reopen on JSONL and SQLite, exact closed-session send refusal, reopen
clearing `closed_at`, concurrency, rollback, sender proofs and armed admin
requirements. PostgreSQL lifecycle integration explicitly SKIPPED because
`CRIER_TEST_POSTGRES_DSN` was not configured; no PostgreSQL result is claimed.

## Real Chromium, explicitly stubbed HTTP

A scratch CDP driver reused the Chrome-pipe setup from
`scripts/chat-a11y-audit.cjs` and loaded this worktree's complete HTML/CSS/JS.
All HTTP responses in that driver were labelled fixture data, not a live server.
Actual browser observations in `/tmp/df-crier-460-browser.log`:

- Armed: applicable control visible and `disabled:true`; visible bound-principal
  reason; native `.click()` produced zero POSTs.
- Unarmed: native click POSTed exactly
  `{"state":"closed","sender":"atlas"}` and the subsequent read rendered
  `closed`; native Reopen restored `open` through the fixture's read surface.
- Racing 403: result class `note management-read refused`, no `error` class;
  full text included `HTTP 403: SESSION_STATE_FORBIDDEN — admin on the session is required to close or reopen it`.
- Uncaught page errors: empty list.

Screenshots (all explicitly labelled as stub HTTP, not live deployment):

- `screenshots/df-crier-460-armed.png`
- `screenshots/df-crier-460-armed-detail.png`
- `screenshots/df-crier-460-refused.png`

The full armed screenshot was visually reviewed: the reason wraps inside the
session header without clipping or overlapping controls.

## Broader short-suite observations

The additional command `go test -p 4 ./... -count=1 -short -parallel 4`
was run twice before commit:

1. `/tmp/df-crier-460-full-suite.log`: exit 1. The documentation route scanner
   treated a dotted field reference as an API path. Fixed the prose to name the
   `management_surface` field from `GET /status`; no scanner allowlist or gate
   was weakened.
2. `/tmp/df-crier-460-full-suite-green.log` (filename was selected before the
   outcome): exit 1. The documentation failure was gone. Unmodified
   `TestSessionFanoutBudgetMixedOutcomes` and
   `TestSessionFanoutAllTimeoutsRunConcurrently` hit their HTTP client deadline.
   Both then passed in isolation with
   `go test -p 4 ./cmd/server -count=1 -short -parallel 4 -run '^TestSessionFanout(BudgetMixedOutcomes|AllTimeoutsRunConcurrently)$' -v`;
   transcript: `/tmp/df-crier-460-fanout-isolated.log`. No network budgets,
   test thresholds, unrelated handlers or guard configuration were changed.

These unsuccessful broad runs are disclosed, not relabelled as a full-suite pass.

## Boundaries

No deployment, push, merge or coordinator-board edit was performed. No live
armed-container browser interaction or new container-restart experiment was
performed. The local existing lifecycle tests, not the browser fixtures, provide
the persistence/event evidence. Browser authentication remains the unresolved
owner decision DF-CRIER-333. No screen-reader or full accessibility-conformance
assessment is claimed.
