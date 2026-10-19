# DF-CRIER-367 — composer state verification

The embedded client derives `{usable, reason}` in `composerState()`. Both control
disabling and the visible `#composer-why` status use that result. The textarea
references `composer-hint composer-why` through `aria-describedby`. Existing
`composerReason()` action guards delegate to that same derivation; the send
implementation, delivery outcome logic, runtime posture renderer and capability
matrix are unchanged.

## Rendered states

- No identity: `Set an acting agent ID (Connection & identity, left) to enable sending.`
  The inline Set acting ID button reveals the identity section and focuses the field.
- Identity, no channel: `No channel open — pick or create one.`
  Inline Pick a channel focuses the session search; Create channel reveals and
  focuses the existing form. The test creates a real channel through this control.
- Signature required: `Send authority: agent signatures REQUIRED. This browser cannot sign; sends are refused as SESSION_SENDER_SIGNATURE_REQUIRED and record nothing. Use an authorized signed client; browser authentication remains an open owner decision (DF-CRIER-333).`
  No browser signing, bypass or new login mechanism is introduced.
- Send pending: `Send or session operation in progress — draft kept. Wait for it to finish.`
  The test delays the response to a real accepted message POST, verifies the draft
  remains intact while disabled, then releases it and checks a single recorded row.

Unknown signature policy fails closed with an inline Refresh runtime posture
button. A failed status read clears stale permissive policy. Closed channels
state that sending is unavailable and offer pick/create recovery too.

## Browser test

`TestChatClientComposerStateBrowser` runs `scripts/chat-composer-state.cjs`
against two real scratch routers (signatures disabled / required), real registered
agents and real channel APIs. Every observation checks that disabled textarea and
send button agree with a visible, nonempty reason, and that the textarea describes
that reason. Ready and completed states require no reason and enabled controls.
It also checks identity/search focus, channel creation, policy recovery, no page
errors, and zero page overflow at 390, 768 and 1280 pixels. A closed-session case
is explicitly injected UI state, not a claimed server-side close operation.
The failed `/status` response and delayed send response are test-only injections.

Run from the repository root:

    NODE_PATH=/tmp/crier-browser/node_modules go test ./cmd/server -run '^TestChatClientComposerStateBrowser$' -count=1 -timeout 120s -v

Install Playwright outside the repository if needed. `CHROME_BIN` overrides the
default `/usr/bin/google-chrome`. Missing prerequisites produce a Go test SKIP,
not a claim of browser verification. The recorded run used real Chromium.
`CHAT_COMPOSER_EVIDENCE` writes screenshots and `browser-results.json` to an
explicit directory. Committed screenshots are under `docs/screenshots/df-crier-367/`.

## Pre-change and mutation controls

Baseline: `543c0467f47fb06c67e13dda8bdf559ab0633493:cmd/server/chat.html`.
The brief's original silent no-channel defect is NOT present verbatim at this
baseline: the DOM already says `Select a conversation to send into.`. It lacks
inline recovery and the textarea's description link, while a locked deployment
incorrectly leaves the composer enabled with no composer reason.

The new browser assertions failed against that untouched baseline before edits
(`/tmp/df-crier-367-red.log`). Replaying the final harness with that historical
HTML also exits 1 (`/tmp/df-crier-367-prefix-final.log`), reporting missing recovery,
missing description wiring and the expected-disabled authority failure. To repeat:

    git show 543c0467f47fb06c67e13dda8bdf559ab0633493:cmd/server/chat.html > /tmp/df-crier-367-prefix.html
    CHAT_COMPOSER_HTML=/tmp/df-crier-367-prefix.html NODE_PATH=/tmp/crier-browser/node_modules go test ./cmd/server -run '^TestChatClientComposerStateBrowser$' -count=1 -timeout 120s -v

`CHAT_COMPOSER_HTML` is a disclosed test-only HTML interception; APIs still use the
real scratch routers. Normal runs exercise the actual embedded page.

A second control blanks only the rendered reason text in a copy of the fixed
HTML. It exits 1 and explicitly reports `disabled iff a non-empty reason is
visibly rendered` for no identity, no channel, pending send, missing authority,
unknown authority and closed channel (`/tmp/df-crier-367-empty-reason.log`). This
proves the disabled-without-reason assertion is not vacuous, without pretending
that an already-fixed historical symptom still exists. Fixed embedded HTML exits
0 with all assertions complete (`/tmp/df-crier-367-green-final.log`).

## Verification

- `go build ./...`: passed (package concurrency bounded to four).
- `go vet ./...`: passed (package concurrency bounded to four).
- `go test ./cmd/server -count=1 -timeout 600s`: passed, 94.489s, with Playwright available.
- The locked router still refuses unsigned sends with 401
  `SESSION_SENDER_SIGNATURE_REQUIRED`; transcript readback contains zero messages.
- Screenshots include no-channel full page/detail, pending send and missing authority.

No board updates, deployment, push or merge is part of this worker change.
