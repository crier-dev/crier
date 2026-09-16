# 2026-09-16 — docs-sweep T7 close-out (CR-GAP-061), with the CR-GAP-053 umbrella ledger

## 1. Header

| field | value |
|---|---|
| date | 2026-09-16 |
| repo | crier — branch `main` |
| commit under record | `4eecc9c` (the docs correction this close-out records) |
| repo HEAD at write time | `c6048d4` (a gitreins evaluator-budget rung the foreman raised so this task's tier-2 re-judge could run) |
| scope | docs-sweep CR-GAP-053 close-out record; evidence produced by tick 283 |
| durable artifact for | tier-2 verdict `d4344c19` on `4eecc9c`, whose only open finding was that the umbrella ledger existed solely in an untracked scratch log |

**Why this file exists.** Verdict `d4344c19` accepted everything in `4eecc9c` — the
gate re-run, the live 502 and 504 correction, the six-item ledger with per-item
file:line evidence — but failed the task on durability: the CR-GAP-053 umbrella
ledger lived only in a scratch log outside the repo, so the finding could not be
re-found by anyone reading the repository. This file is the repo-tracked copy of
that record. Every number below was produced by a command run in this tree at
`c6048d4`; nothing is copied forward from the earlier capture without a fresh
re-run, and the two places where a re-run disagreed with the earlier capture are
called out explicitly (the `xfail` count in §5, and the sink payload length in §4).

Two scratch logs back this record: the tick's original transcript
`crier-tick283-t7-proof.txt` and this rework's fresh re-runs
`crier-tick283-t7-rework-proof.txt` (both in the host temp directory, untracked by
design — this file is their durable summary). This file is not referenced by any
claim in `docs/claims.yaml`, so the gate's scanned-path pass never reads it; it is
still written without leading-slash path tokens or host:port tokens so the path
scanner cannot learn a new token from it.

## 2. Sweep map — T1..T7 (and the T1b recipe-replay detector)

Verdict ids are the `.gitreins/history/2026-09-16/<id>/` directories. A task's
row commit is the one the board records; several tasks were judged more than once,
so the table names the attributable verdict (the latest run on the same line, with
the row commit an ancestor of the revision that was judged) and counts the earlier
failed runs instead of hiding them.

| rung | board row id | what it landed | commit | attributable verdict id | tier2 | earlier failed runs |
|---|---|---|---|---|---|---|
| T1 | CR-GAP-055 | the claim-executing docs gate: `cmd/server/docsclaims_test.go` + `docs/claims.yaml` + `make docs-check` | `3757494` | `1b22b2d6` | PASS | 0 |
| T1b | CR-GAP-062 | recipe replay: doccheck-marked fenced blocks executed against a live in-process server | `07b7304` | `6342ae67` | PASS | 0 |
| T2 | CR-GAP-056 | TESTERS.md 5-minute path rewritten so it runs verbatim; rough-edges table rebuilt from live evidence | `ae38bbc` | `50af7187` | PASS | 2 (`ac905274`, `7ad801dc`) |
| T3 | CR-GAP-057 | README.md truth pass: static-docs wording, auth-exemption list, endpoint counts, 13-tool MCP table | `5e47c7c` | `7515140c` | PASS | 2 (`ff564a78`, `74c0589f`) |
| T4 | CR-GAP-058 | `docs/openapi.yaml` reconcile + the `go:generate` embedded-copy step | `980ae5b` | `29a27d37` | PASS | 1 (`4e647851`) |
| T5 | CR-GAP-059 | `docs/architecture.md` + `docs/specs.md` truth pass (mesh pointer fixes) | `4764fef` | `274bf563` | PASS | 0 |
| T6 | CR-GAP-060 | `examples/` + agent-facing prose; the gateway-demo transcript refreshed from a real run | `53cd87f` | `17b60c0c` | PASS | 3 (`c204872c`, `2a8a26a5`, `e66fa48b`) |
| T7 | CR-GAP-061 | this close-out: full suite + gate re-run, residual 504 drift corrected, umbrella ledger | `4eecc9c` | `9be76f1b` | FAIL (the verdict this rework answers) | 0 |

Only T7's verdict is a FAIL, and its stated reason was durability of the ledger —
not any factual defect in `4eecc9c`. The earlier failed runs on T2, T3, T4 and T6 were
each followed by a foreman-raised evaluator budget rung and a re-judge that passed;
that sequence is visible in the history directories themselves.

**Not attributable.** No other verdict directory under
`.gitreins/history/2026-09-16/` carries a docs-sweep task id, so there is nothing
else to attribute; the sibling tasks in that directory (`INT-CI-004`,
`DF-CRIER-183`, `DF-CRIER-184`, `DF-CRIER-187`, `DF-CRIER-178` and `DF-CRIER-152`, the cr-gap-058, cr-gap-059 and cr-gap-060 re-runs) belong to other rows.

## 3. CR-GAP-053 umbrella ledger — the six acceptance items

Re-verified against the current tree (`c6048d4`), not against the tick's capture.
Every line number below was re-read with `sed -n <line>p` in this tree; the tick's
own edit to `docs/dogfood/diagnostics.md` was the reason the brief demanded this
re-check, and every cited line still resolves to the quoted text.

**ITEM 1 — all eight delivery modes documented with a section and a worked
request-and-response example. Verdict: PASS.**

| mode | README.md | TESTERS.md | also |
|---|---|---|---|
| blocking | `:74` §5 heading, `:88` status row | `:158` | `specs/WEBHOOK-DELIVERY.md:106` (mode table: "POST + wait, reply from body" → `200 {id,transport,reply,…}`) |
| async | `:89` status row | `:158` | `specs/WEBHOOK-DELIVERY.md:107` (202 accept plus queue and retry semantics) |
| batch | `:89` status row, `:456`/`:457` (`CR_WEBHOOK_BATCH_MAX` 10, `CR_WEBHOOK_BATCH_FLUSH_S` 5) | `:158` | `specs/WEBHOOK-DELIVERY.md:108` |
| inbox-lease | `:65` §4 heading, `:69`–`:70` lease and ack rules | `:80` | `docs/integration-guide.md:119` §3 (deliver → retrieve → ack) |
| relay | `:29` §1 heading | `:131` | `docs/integration-guide.md:288` §4 |
| mesh | `:39` §2 heading | `:198` | `docs/integration-guide.md:307` §5; `docs/mesh-protocol.md` |
| federation | `:427` (`CR_FED_LINKS` semantics incl. the held-202 path), `:478` (the federation peer-listing route row) | `:208` | — |
| MCP | `:260` "Remote MCP mode (crier-mcp)", `:316` "#### MCP tools" | `:226` | `docs/integration-guide.md:354` §6.1 |

Evidence commands: `sed -n '<line>p' README.md TESTERS.md specs/WEBHOOK-DELIVERY.md
docs/integration-guide.md` for each row above (28 line reads, all matching).

**ITEM 2 — the base64 inbox contract (DF-CRIER-30) is stated where a tester needs
it. Verdict: PASS.**

- `README.md:232` — "# Note: message payloads are base64-encoded on the wire ([],byte form)"
- `docs/integration-guide.md:233` — "- The payload is base64-encoded JSON — decode before use."
- `TESTERS.md:99` — the decode one-liner (`base64.b64decode('eyJoZWxsbyI6ImJvYiJ9')` → `{"hello":"bob"}`)

**ITEM 3 — relay publish `X-Agent-ID` requirement present in the quickstart.
Verdict: PASS.**

- `README.md:206` — "# 3. Publish to a relay topic (publish and subscribe). `X-Agent-ID` is the header the relay …"
- `README.md:215` — the publish curl carries `-H 'X-Agent-ID: agent-1'`
- `README.md:218` — "# 202 — drop the trio and it is still 202; drop `X-Agent-ID` and it is 401."

**ITEM 4 — the wildcard-subscriber doc line is actually implemented. Verdict: PASS
(doc `TESTERS.md:253`–`:255` + live probe, re-run for this record — see §4).**

**ITEM 5 — MCP tool inventory current (13) and an onboarding path. Verdict: PASS,
with one open drift.**

- `README.md:318` — "The MCP server exposes **13 tools** (measured live via a tools-list stdio …"
- `README.md:316` — "#### MCP tools" (the inventory table follows)
- `TESTERS.md:226` / `:231` — "6. MCP mode (for agent frameworks)" … "13 tools"
- `docs/integration-guide.md:354` — "### 6.1 Remote MCP mode (crier-mcp against a running server)"
- Live measurement in the tick (an MCP initialize followed by a tools-list request over stdio): exactly 13 tools
  (`register_agent`, `list_agents`, `get_agent`, `unregister_agent`, `deliver_message`,
  `retrieve_inbox`, `ack_messages`, `inbox_stats`, `send_message`, `get_messages`,
  `ask_agent`, `mesh_peers`, `mesh_request`).
- FINDING (does not fail the item): `docs/integration-guide.md:351` still reads
  "(`make build-mcp`, stdio, 8 tools)" while live is 13 — owned by
  DF-CRIER-39, see §6 residual R1.

**ITEM 6 — `docs/openapi.yaml` verified against the handlers. Verdict: PASS.**

- `cmp cmd/server/openapi.yaml docs/openapi.yaml` → exit 0, byte-identical (the
  generated copy is the source; the `go:generate` directive in `cmd/server/openapi.go`).
- `go test -short -count=1 -run TestOpenAPI ./cmd/server` → `TestOpenAPIServed` PASS,
  `TestOpenAPIDocsSpec` PASS (the byte-identical assertion is part of the suite).

## 4. Live probe evidence behind `4eecc9c` — 502 / 504 split and the wildcard trio

Both halves were re-measured on the current tree for this record; the raw transcript
of the fresh run is appended to `crier-tick283-t7-rework-proof.txt`. Each probe port
was asserted free before start and released after teardown, and the listening pid was
asserted equal to the pid the harness started — the failure mode that assertion exists
to catch is real: this rework's first attempt aborted on that very check because the
harness compared the listener against its own subshell pid instead of the binary's
(the harness now `exec`s the server). Nothing in the aborted attempt was used as evidence.

**The split, as documented in code.** `internal/registry/handler.go:602` writes
`http.StatusBadGateway` for a `webhook.ErrPermanent` failure (the block's comment at
`:598`–`:601` says the 2xx body that the reply schema cannot map "can never succeed on
retry"), and `:605` writes `http.StatusGatewayTimeout` for everything else in that
branch (timeout / budget exhaustion, which "may still land later").

**Direction 1 — permanent, unmappable 2xx → 502.** Agent `wb-502` registered with
`openai-compatible` + `response_map` `choices.0.message.content`, pointed at a sink
that answers `200 {"nope":{}}`; a POST to the wb-502 inbox route with
`{"delivery_mode":"blocking"}` returned:

    HTTP 502
    {"error":"webhook: permanent failure: reply extraction: response map
     \"choices.0.message.content\": missing key \"choices\""}

That body is the load-bearing part: the status alone could be a proxy, the exact
missing-key path proves the reply-extraction failure was classified permanent.

**Direction 2 — timeout / budget exhaustion → 504.** Agent `wb-504` pointed at a sink
that sleeps 3.0 s; the same deliver with `"timeout_ms":500` returned:

    HTTP 504
    {"error":"webhook: blocking delivery timed out after 500ms (last: post
     <sink-B hook path>: Post \"<sink-B hook path>\": context deadline exceeded)"}

(the hook path itself is elided here; the unredacted body is in the scratch log)

Both POSTs really reached their sinks (the sink logs record one POST on the wb-a hook
path and one on the wb-b hook path), so neither status is a number produced without a
delivery attempt. One honest discrepancy against the tick's capture: the logged
payload length is 357 bytes here versus 134 in the tick's run — the difference is
the request payload the two runs sent, not the delivery path. A second, expected
artifact: the sleeping sink logged a `BrokenPipeError` when it tried to write its
200 after the client had already given up at the 500 ms budget — that traceback *is*
the timeout direction, not a product failure.

**Wildcard trio (doc `TESTERS.md:253`–`:255`), driven with the repo's own
`examples/ws-mesh-demo` subscriber against a scratch server:**

| case | publish | subscriber | observed |
|---|---|---|---|
| A | `alerts.fire` | `alerts.*` | `SUBSCRIBED alerts.*` then `EVENT {"msg":"to alerts.fire"}` — receives |
| B (negative) | `alerts.fire.deep` | `alerts.*` | `SUBSCRIBED alerts.*`, no EVENT after 2 s — one segment only, exactly as documented |
| C | `alerts.fire.deep` | `alerts.>` | `SUBSCRIBED alerts.>` then `EVENT {"msg":"to alerts.fire.deep"}` — receives |

Case B is what makes A and C non-vacuous: the same topic, publisher and server
produce no event for the one-segment pattern, so A and C are not "everything
receives everything".

**Port discipline (both parts).** Every port was asserted free by `ss` before start
and asserted released after teardown; the listener pid equalled the started pid in
both parts. Teardown was by pid, and the harness additionally kills any port owner
on an abort path — an earlier abort had leaked a listener exactly because a
subshell was killed instead of its child.

## 5. Gate + non-vacuity evidence (re-run for this record)

All of the following was re-run on the tree carrying this record, at HEAD `c6048d4`
(this file's sha256 at that run: `a89f82c87ab5a5940e0f93fb45bea14f99cfc9bb4ade54e9513d09056fa7b90d`).
Nothing below is copied forward from the tick's earlier capture. Raw output:
`crier-tick283-t7-rework-proof.txt`.

**`make docs-check` — rc=0.**

    go test -short -count=1 -run 'TestDocsClaims' ./cmd/server
    ok  github.com/crier-dev/crier/cmd/server  0.124s

**Non-vacuity — the gate executed work, and its own detector is falsifiable.** Read
off the `-v` run, not inferred from the exit code:

- `docsclaims_test.go:545: recipe replay: docs=6 executed_steps=5 verdicts=5`
  — six docs read, five steps executed against a live in-process server, five
  verdicts asserted. A gate that read nothing and asserted nothing would report zeros.
- Subtests all PASS: `anchors`, `routes`, `defaults`, `counts`, `recipes`.
- `TestDocsClaimsDetectorNegativeControl` PASS, with its case table printed:
  `(a) block whose curl step carries no verdict reported=true` (a finding is raised
  when none should be), `(b) wrong verdict (claimed=404 observed=200) reported=true`,
  `(c) correct block clean=true requests_issued_against_live_server=3`,
  `(d) marker with no fenced block reported=true`, and
  `recipe replay negative control: executed=3 verdicts=2 live_server_requests=3 findings=3`.
  So the detector fires on bad input, stays quiet on good input, and really speaks to
  a live server while doing it.

**`go test ./... -count=1 -timeout 60s` — rc=0, 13 packages ok, 0 FAIL:**
`cmd/crier-mcp`, `cmd/server`, `config`, `examples/ws-mesh-demo`,
`internal/buildinfo`, `internal/federation`, `internal/guard`, `internal/mcp`,
`internal/mesh`, `internal/middleware`, `internal/registry`, `internal/relay`,
`internal/webhook`.

**`go vet ./...` — rc=0, no output.**

**xfail census — 28 claims, 0 pinned.**
`grep -c '^    xfail:' docs/claims.yaml` → 28; `grep -c '^  - id:'` → 28;
`grep -cE '^    xfail: [^n]'` → 0 (no claim is pinned to a board id, i.e. zero xfail).
One discrepancy against the earlier captures is worth recording: the raw
`grep -c 'xfail: null'` returns **29**, not 28, because line 23 of the file is the
field's own explanatory comment (`#   xfail: null (claim must HOLD live) or "<BOARD-ID>" …`).
The verdict text for `4eecc9c` reported "29 `xfail: null`" for that reason; this
record states the re-measured, field-scoped number (28 claims / 28 values / 0 pinned)
and names the extra hit rather than hiding the difference.

**Negative control — a green gate that cannot fail is not evidence.** The
load-bearing status claim `WEBHOOK-DEFAULT-BLOCKING` (`docs/claims.yaml:265`,
`expect: "202"` against `specs/WEBHOOK-DELIVERY.md`'s delivery-mode line) was
falsified in place (`expect: "599"`), the gate re-run, and the file restored:

- with the falsified expectation: `make docs-check` → **rc=2, FAIL**:
  `docsclaims_test.go:328: claim WEBHOOK-DEFAULT-BLOCKING [status]: doc=specs/WEBHOOK-DELIVERY.md quote="- `delivery_mode` — `async` (default) | `blocking` | `batch`." expected=599 observed=202`
  and `--- FAIL: TestDocsClaims/routes`
- restored byte-identically: `sha256sum docs/claims.yaml` →
  `a7b85a3f2afa94e5ae6b35c1354d641eed60035832cc2e8151cb005fdda0b0ce` before **and**
  after (identical to the hash recorded by the earlier tick), `git diff` empty,
  `git status --short` empty, and `make docs-check` → rc=0 green again.
- The falsification exists only in the scratch log; the committed
  `docs/claims.yaml` is untouched.

**Re-run after finalization.** Once this section's text, the section numbering and one
verbatim-body redaction were final, the gates were run a second time against the file
as it then stood and reproduced every result above: `make docs-check` rc=0, the full
suite 13/13 ok, `go vet ./...` rc=0, the census 28 claims / 28 values / 0 pinned, and
`docs/claims.yaml` still at `a7b85a3f…`. Both runs' raw output, and the sha256 of the
file at each run, are recorded in `crier-tick283-t7-rework-proof.txt` and in this
commit's body. Nothing in this section is inferred from a green exit code alone: the
non-vacuity counters, the detector's own case table and the negative control are all
quoted from the run output.

## 6. Residuals this sweep did NOT close

Each residual names its file:line **and** the owning board row's id *and* title,
because row ids are reused on this board (`DF-CRIER-1` appears on 11 rows,
`QA-CRIER-1` on 7, `CR-GAP-053` on 3), so an id alone is ambiguous.

| # | residual (file:line) | docs-fixable? | owning board row (id + title + status) |
|---|---|---|---|
| R1 | `docs/integration-guide.md:351` — "(… stdio, 8 tools)" while live is 13 | yes | **DF-CRIER-39** — "MCP surface doc drift: guide says 8 tools (live: 13), shared-backend claim false in default in-memory mode" — pending |
| R2 | `docs/integration-guide.md:288`–`:301` — the relay publish example omits `X-Agent-ID` | yes | id **DF-CRIER-1** — that id is on 11 rows; the row matching this residual is titled "Relay publish docs gap: no README curl example, and the integration-guide example omits X-Agent-ID" — pending |
| R3 | `docs/integration-guide.md` — no webhook and federation section (sections stop at `## 6. Durable setup` / `## 7. Full round-trip`) | yes | **DF-CRIER-10** — "No webhook/federation section in docs/integration-guide.md — push delivery is spec+openapi only" — pending |
| R4 | `docs/dogfood/diagnostics.md:131`–`:132` — per-agent `retries` ignored, cadence 30 s not `CR_WEBHOOK_REDELIVER_S` | **no** — code behaviour, not prose | **DF-CRIER-9** — "Per-agent webhook `retries` setting is ignored — server max(5,2*agent_retries) attempts used instead" — pending |
| R5 | `docs/dogfood/diagnostics.md:123`–`:124` — "No `CR_FED_*` secret env exists" | yes (stale dated capture) | owning row **DF-CRIER-6** — "Federation forwards deliveries WITHOUT auth credentials — link to an auth-enabled relay 401s" — **complete**; `CR_FED_TOKEN` now exists (`README.md:429`). No open row: this is a future docs truth-pass item, not an engine row |
| R6 | `docs/dogfood/diagnostics.md:140`–`:144` — "Registration validation order quirk (not filed)" | **no row recommended** — see below | **none** (by design) |

**R6 — the one residual with no row, and why that is the finding.** The dated
capture records a bad `public_key` "erroring the same with and without a webhook
object" as if it were a registry bug. The live re-check in the tick showed the
behaviour is consistent and by design: a bad `public_key` returns
`400 {"error":"public_key must be 64 hex characters (ed25519)"}` with **and** without
a webhook object; with a well-formed key a bad webhook URL returns
`400 {"error":"webhook.url must be http(s)://"}`; and neither agent registers
(`GET` → `404 agent not found`, the very 404 the original author mistook for a
registry bug — the 400 body had been swallowed by discarding the response body). The key is
validated before the webhook, always with the exact reason in the body. That is a
UX observation about a diagnostic, not a defect: **no engine row recommended.** The
foreman owns filing either way; this record deliberately leaves it unfiled rather
than manufacturing a row.

## 7. Row-closure note

The three umbrella rows carrying id **CR-GAP-053** ("docs completeness sweep — every
delivery mode documented with wire examples; known doc drifts closed") were each
`status=pending` at the time of the tier-2 verdict that asked for this artifact. They
are closed by the foreman in the same tick (tick 283) as board work, and at the time
of writing all three read `status=complete` with `commit_hash=4eecc9c`. This file is
their evidence pointer: the six-item ledger above, the gate results in §5, the live
probe evidence in §4, and the residuals in §6 (including the deliberate "no row
recommended" for R6). The docs-sweep row **CR-GAP-061** itself is likewise
`status=complete` at `4eecc9c`, and **DF-CRIER-172** (the four stale 504 doc lines)
is `status=complete`.
