# Chat panel browser evidence — 2026-10-27 (DF-CRIER-335 iteration)

Produced by the real harness, not by hand: `node scripts/chat-ui-browser.cjs` against a
stock `go build ./cmd/server` binary, real headless Chromium at `/usr/bin/google-chrome`,
scratch JSONL sessions and an in-memory registry (no production records).

    NODE_PATH=/tmp/crier-browser/node_modules \
    CRIER_TEST_BINARY=/tmp/cr-chat-038-server \
    CHAT_EVIDENCE=<dir> node scripts/chat-ui-browser.cjs
    # exit 0: "PASS: full browser suite and 3 viewport checks"

## Responsive contract — 390 / 768 / 1280

Measured `scrollWidth == viewport` with zero overflowing elements at every width
(`browser-results.json` → `measurements`):

| viewport | scrollWidth | overflows | transcript last message |
| -------- | ----------- | --------- | ----------------------- |
| 390      | 390         | 0         | visible, scrolled to it  |
| 768      | 768         | 0         | visible, scrolled to it  |
| 1280     | 1280        | 0         | visible, scrolled to it  |

Screenshots: `chat-390.png`, `chat-768.png`, `chat-1280.png`.

## Auth boundary — DF-CRIER-333

`browser-results.json` → `authBoundary`:

| probe                                    | result |
| ---------------------------------------- | ------ |
| `GET /chat` with no header (Node fetch)  | 401    |
| `GET /chat` with the Bearer (Node fetch) | 200    |
| **browser** navigation, no front         | 401 + the DF-CRIER-444 fronting-requirement page, no panel (`chat-authenticated-401-no-front.png`) |
| **browser** navigation through a front   | 200, the real panel (`chat-authenticated-front-1280.png`) |
| the panel's OWN protected `GET /sessions` | returned the record created with the token — the front's header reached the real server |
| token in page source / localStorage / sessionStorage / cookies | absent |

The front in that arm is a process the harness starts: it holds the credential in its own
memory and adds the header to every upstream request. **Which fronting arrangement ships is
still the open owner decision (DF-CRIER-333)** — this evidence proves the acceptance's
browser requirement, it does not take that decision.

## What this does not claim

- No agent runtime executes a task; Complete supplies observed harness evidence.
- Capability matrix, transport failures, hostile responses and storage denial use
  browser-intercepted test responses.
- `CHAT_MATRIX_ONLY=1` runs a subset and says so.
