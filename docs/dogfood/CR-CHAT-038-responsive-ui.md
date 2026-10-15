# CR-CHAT-038 — responsive operator chat verification

## Scope

The product UI remains one self-contained document, `cmd/server/chat.html`,
embedded by `cmd/server/chatclient.go` and served at `GET /chat`. No server route,
auth middleware, delivery implementation, storage contract or backend policy changed.
The visual baseline is `docs/mock-ui-cr-chat-011.html`, not the original generated
reference images (which are not available). No pixel-perfect comparison is claimed.

The mockup's charcoal/mint shell now has a searchable conversation index,
connection/attribution controls, conversation header, selectable thread view,
reply controls, composer, participants/presence, current tasks, permissions and
capability directory. At widths below 900px the zones stack. Long message bodies
wrap rather than forcing horizontal scrolling. There is no page-level overflow
hiding used to disguise clipping.

## Data and behavior contracts

All product records are HTTP responses, never bundled demo fixtures:

| UI | Existing public API |
| --- | --- |
| Sessions, create channel | `GET /sessions`, `POST /sessions` |
| Transcript, message/reply | `GET /sessions/{id}/messages`, `POST /sessions/{id}/messages` |
| Membership and presence | `GET /sessions/{id}/participants`, `GET /agents` |
| Permissions | `GET /sessions/{id}/permissions` |
| Task lifecycle read, explicit task create | `GET /sessions/{id}/tasks`, `POST /sessions/{id}/tasks` |
| Advertised capabilities | `GET /agents` |
| Health and aggregate presence | `GET /status` |

Replies carry the exact `parent_id`; thread filtering uses the server's
`thread_id`, not recursive visual indentation based on reply count. Plain,
addressed and task messages have distinct rendering. Addressed messages have no
execution affordance. Only selecting **Explicit task**, choosing an explicit
agent recipient and submitting **Create task** uses the task endpoint.
Per-recipient outcomes are displayed even when HTTP 201 includes a refusal.
A missing delivery outcome is explicitly unconfirmed, not displayed as delivered.

The recipient dropdown controls explicit agent routing. Capability advertisements
are read-only here: they are not grants, and this composer does not claim to route
a capability tag through the session endpoint. Capability delivery's existing
one-holder selector is not represented as a broadcast. Typed address syntax is not
reimplemented as a second routing/parser system in the browser.

Refresh is explicitly **HTTP polling every 15 seconds while visible**, with a
manual refresh control. The stock relay/mesh sockets are not a session transcript
feed; the UI does not invent a subscription topic/protocol or claim a WebSocket
connection. Poll results are generation-guarded against session/identity races.

Unavailable actions/data are stated as unavailable: permission writes, attachment
operations, capability routing in this composer, guard verdicts/lease acknowledgments
in this transcript, and task progress percentages. A task state comes only from
its recorded lifecycle, never from parsing prose. Registry absence is “presence
unavailable,” not a fabricated offline/online badge. Permission maps use the
server's false-by-absence boolean-map semantics; missing response structures do
not grant permissions.

## Authentication blocker (unchanged)

`internal/middleware/auth.go` exempts only `/health`, `/version`, `/openapi.json`,
`/openapi.yaml`, and `/docs`. With `CR_AUTH_TOKEN` configured, `/chat` itself and
its API calls require `Authorization: Bearer ...`. There is no browser login,
OIDC exchange or cookie session in the existing API. An ordinary navigation
cannot supply the custom Bearer header, and a loaded page cannot obtain a safe
per-user credential from any existing exchange.

The client therefore does not collect, embed, persist, put in a URL, or forward
a shared message/admin token. Agent ID is explicitly attribution, not login.
The namespace input is a scope header, not a secret. A token-protected namespace
requiring `X-Crier-Namespace-Token` likewise has no credential entry flow here;
its actual refusal is shown. Production browser sign-in is blocked pending an
approved external access arrangement or a separate auth contract; no policy was
weakened to make this task appear successful.

The browser battery starts a second stock scratch server with an ephemeral test
Bearer token: `GET /chat` without a header returns **401**, and the same read with
the correct header returns **200** with the rebuilt document. This is an HTTP
boundary check, **not a claim of a working browser login**. The interactive UI
checks use a stock development-mode scratch server, not the production service.

## Real-browser verification

Engine: Google Chrome 152.0.7977.82, headless Chromium driven by Playwright.
The test rebuilds/uses a stock Go binary with the embedded page, creates actual
registered test agents and JSONL session records in uniquely allocated scratch
storage, and reads the records back from the API. No production data is changed.

Measured with populated state, an 800-character unbroken message, literal
HTML/event-handler text, replies, an explicit task and a real refused delivery:

| Viewport | document.scrollWidth | body.scrollWidth | Horizontal overflow |
| --- | --- | --- | --- |
| 390px | 390px | 390px | 0px |
| 768px | 768px | 768px | 0px |
| 1280px | 1280px | 1280px | 0px |

The battery also checks descendant scroll/client widths (excluding intentional
single-line preview ellipsis and native input internals), bounding boxes of all
composer controls, and scroll reachability of the final message. **Zero page
JavaScript errors.** Desktop transcript scrolling is intentional; mobile presents
the complete thread vertically. Screenshots are not proof of data delivery;
the API readbacks below are the proof.

Verified behavior:

- Actual sessions, participants, registry presence/capabilities and permissions render.
- Independent threads are isolated by the filter; a submitted reply is read back
  with the original parent and thread IDs.
- An addressed message creates zero tasks. Explicit task intent without a recipient
  makes no request; explicit recipient/task submission creates one recorded task,
  rendered in the rail and as a distinct transcript card.
- A correctly signed unregister request removes the selected recipient; that
  recipient is not silently replaced by a broadcast. The subsequent message's
  actual refusal appears in both the immediate UI result and the API transcript.
- Browser-intercepted **test-only** 401, 403 and 500 write responses display their
  errors and retain the draft. These fault responses are not represented as live
  server-produced errors. A read failure disables sending and displays its error.
- Delayed old-session responses cannot overwrite a newly selected conversation.
- Malicious-looking message text and attribute-shaped IDs remain inert text,
  with no injected image/event attribute or JavaScript execution.
- Channel creation through the browser form is confirmed by `GET /sessions`.
- Empty search and session-read auth failure remain distinguishable.

Evidence (all records shown are identified scratch test data):

- `../screenshots/cr-chat-038/chat-390.png`
- `../screenshots/cr-chat-038/chat-768.png`
- `../screenshots/cr-chat-038/chat-1280.png`
- `../screenshots/cr-chat-038/chat-composer-detail.png`
- `../screenshots/cr-chat-038/browser-results.json`

## Reproduce

```sh
go build -p 4 ./...
go vet -p 4 ./...
go test -p 4 ./... -count=1 -parallel 4

go build -p 4 -o /tmp/cr-chat-038-server ./cmd/server
npm install --prefix /tmp/crier-browser --no-audit --no-fund playwright
NODE_PATH=/tmp/crier-browser/node_modules node scripts/chat-ui-browser.cjs
```

`CHROME_BIN` can override `/usr/bin/google-chrome`; `CRIER_TEST_BINARY` can override
the scratch binary path; `CHAT_EVIDENCE` selects a directory outside the source
tree for browser artifacts. The harness clears inherited `CR_*` / `CRIER_*`
settings before launching its servers, preserves the stock signature/auth rules,
and shuts down only the server children it started.

The two new embedded-shell/safety regression tests were also run against the
historical `7e5afcd` HTML using Go's build overlay (no working-tree reset): both
failed on the missing responsive/safety contract. The rebuilt version passes.
These structural checks complement, not replace, the real-browser battery.

The full build, vet and uncached Go suite passed before committing. One additional
repeat returned `504` rather than `200` in the unchanged backend test
`internal/daggerctl.TestHTTPWaitResolveAndStatus` (its wait budget is five seconds).
That result is retained in `/tmp/cr-chat-038-test-timing-failure.log`; no backend
code or test threshold was changed. The final gate result is reported separately.
The delivered commit is re-tested after creation; its SHA, final gate output and
remote parity are recorded in the worker's final report. No production deploy or
main-branch merge is part of this change.
