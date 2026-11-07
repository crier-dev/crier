# CRIER-HARNESS-6 — live delivery into a real Hermes session (evidence)

Run date: 2026-11-06 (host clock 23:49–23:52 -05:00; UTC 2026-11-07 04:49–04:52)
Host: the operator's workstation (`kara`), Hermes **v0.22.5+3030.g770a952 · stable** at
`/home/kara/.local/bin/hermes`, Hermes gateway answering on `127.0.0.1:8642`.
Repo: `github.com/crier-dev/crier`, worktree `~/worktrees/crier-CRIER-HARNESS-6`
(branch `wt/CRIER-HARNESS-6`, based on `origin/main` 1587a03f).

Both legs are env-gated on `CRIER_TEST_LIVE_HARNESS=1`; without it they `t.Skip`, so
`make test` and CI are unchanged.

## The question this evidence answers

Crier's `internal/harness/hermes` adapter claims two verified surfaces:

* continue — `hermes chat -Q --resume <id> -q <text>`
* create — gateway `POST /v1/responses` with no resume id

CRIER-HARNESS-6 asks whether a message handed to that adapter actually lands in a
**real Hermes session**, and whether a **second** message continues that same session
instead of creating another one.

**Answer: yes, via the CLI-resume transport.** The gateway create path was not used
(see "Transport" below) — it needs `adapters.hermes.gateway_url` + a gateway token,
and a deployment that hands Crier an already-bound session id does not need it.

## Where the transcript lives

The delivered text is found in Hermes's own session store:

```
file:   /home/kara/.hermes/state.db        (SQLite, journal_mode=WAL)
table:  messages
key:    session_id = <the Hermes session id>
```

This Hermes (v0.22.5) keeps CLI sessions in `state.db`; the per-session
`~/.hermes/sessions/<id>.jsonl` files are the **legacy** `write_json_snapshots` format
(that knob is marked for removal in this tree) and are not written for CLI sessions.
`hermes sessions export --session-id <id>` reads the same rows. Reproduce the query:

```bash
sqlite3 -readonly /home/kara/.hermes/state.db \
  "select id, role, content from messages where session_id='local-c8aad6a6…' order by id;"
```

`transcript-excerpt.txt` in this directory is that query, run once per session the two
legs created. `-readonly` is used everywhere; nothing in the tests can write to the store.

## Leg 1 — served route: `POST /ingress/messages` (criterion 1, 2, 3)

`cmd/server/hermes_live_delivery_test.go :: TestIngressDispatchesIntoARealHermesSession`

Boots the **real server** (`startTestServerWithEnv`, the shared in-process boot the
whole `cmd/server` suite uses) with the **real hermes adapter registered**
(`CR_HARNESS_ENABLED=true`, `CR_HARNESS_DEFAULT=hermes`,
`CR_HARNESS_HERMES_BINARY=/home/kara/.local/bin/hermes`), seeds one Crier session
(`s_live_hermes`) bound to `harness=hermes` with a freshly created Hermes session id,
then pushes two unique tokens through the route:

```
push m_ingress_live_1: http=200 elapsed=9.698s delivery={Mode:harness Harness:hermes State:dispatched Error:}
transcript evidence: file=/home/kara/.hermes/state.db table=messages
  session_id=local-0537438f7d7261e3ead948f55321fed566f02e29c3d78cd7faee127e16740d88 (4 rows)
  carries crier-live-hermes-ingress-1794027075862845196-one
push m_ingress_live_2: http=200 elapsed=7.988s delivery={Mode:harness Harness:hermes State:dispatched Error:}
continuation proven through the served route: one hermes session "local-0537438f…", 4 -> 6 rows
--- PASS: TestIngressDispatchesIntoARealHermesSession (29.65s)
```

Full output: `ingress-live.log`. The push was admitted with no harness fault: the
server logged `POST /ingress/messages status=200`, and the delivery object answers
`mode=harness`, `harness=hermes`, `state=dispatched`, empty `error`.

## Leg 2 — the adapter path itself (criterion 2, 3, 4)

`internal/harness/hermes/live_delivery_test.go :: TestLiveHermesDeliveryLandsInTheRealSession`

Drives `hermes.New(...).Deliver(...)` directly — the same call `harnessDispatch` makes
on the ingress route (`cmd/server/harnesswiring.go`) — against the same live session
store:

```
setup: created live hermes session "local-c8aad6a6…" (marker crier-live-hermes-1794026957533208286)
delivery m_live_hermes_1: receipt={Harness:hermes SessionID:local-c8aad6a6… Created:false
  State:dispatched ExitCode:0 DurationMS:15821 StdoutBytes:42} elapsed=15.822s
transcript evidence: file=/home/kara/.hermes/state.db table=messages session_id=local-c8aad6a6…
  row_id=6812447 role=user token=crier-live-hermes-1794026957533208286-one
delivery m_live_hermes_2: receipt={Harness:hermes SessionID:local-c8aad6a6… Created:false
  State:dispatched ExitCode:0 DurationMS:16703 StdoutBytes:42} elapsed=16.704s
continuation proven: one session "local-c8aad6a6…", 4 rows before the second push, 6 after
--- PASS: TestLiveHermesDeliveryLandsInTheRealSession (52.95s)
```

Full output: `adapter-live.log`.

Both receipts are `Created:false` for the same `SessionID`, and the session row count for
that id does not change — the second push continues the first session rather than
minting a new one. The transcript excerpt shows why that is the strong form of the
claim: the assistant turn quoting each token sits in the *same* `session_id`, so the
message was **run** by the session, not merely stored next to it.

## Transport (criterion 4)

**CLI resume** delivered both pushes in both legs:

```
hermes chat -Q --resume <hermes-session-id> -q <message text>
```

`internal/harness/hermes/hermes.go:113` builds exactly that argv. The gateway
`POST /v1/responses` create path was **not** used: the adapter only takes it when
`SessionRef.ID == ""`, and a deployment that binds a session records the id first
(`harnessDispatch` persists the minted id as `session.harness.bind`), so every later
delivery continues. `Created:false` in both receipts is the observable proof that the
create path did not run.

## What the tests deliberately do not do

* They never delete, rename or mutate an existing transcript. Each leg creates its own
  fresh Hermes session (via `hermes chat -Q --in <tmp> -q "…SESSION-READY"`, whose
  answer is the session's title) and only ever appends to it through the adapter.
* They open `state.db` with `?mode=ro`. No write path exists in the test code.
* They scrub `HERMES_SESSION_ID` / `HERMES_SESSION_KEY` and friends from the child
  environment for the duration of the run, so the delivery cannot inherit the calling
  worker's own session binding, and restore them on cleanup.
* They skip (never fail) when `CRIER_TEST_LIVE_HARNESS=1` is unset or `hermes` is absent.

The sessions created by these runs remain in the store (nothing is deleted):
`local-c8aad6a6…` and `local-0537438f…`, plus a handful of `…-SESSION-READY` probe
sessions from the development attempts of this same test. They are inert rows in a
54 GB store; removing them was judged riskier than leaving them.

## One observed flake, stated rather than hidden

While developing this leg, one ad-hoc `hermes chat -Q --resume` failed on the first
attempt with `Hermes couldn't start the model connection: Gateway did not answer
session.resume`; the same command succeeded seconds later, and the 2026-11-06
`agent.log` shows `tui_gateway.ws: ws response send failed … method=session.resume`
followed by `send_failed_after_response` — an I/O stall in the CLI's TUI-gateway
bridge on a loaded host, not a delivery refusal. Neither gated leg hit it (all four
deliveries dispatched first try), but a future red on this leg should be read against
that line before being attributed to the adapter.

## Reproduce

```bash
cd ~/worktrees/crier-CRIER-HARNESS-6
go build ./... && go vet ./... && go test -short -count=1 ./internal/harness/...

CRIER_TEST_LIVE_HARNESS=1 go test -count=1 -v -timeout 900s \
  -run 'LiveHermes' ./internal/harness/hermes/

CRIER_TEST_LIVE_HARNESS=1 go test -count=1 -v -timeout 900s \
  -run 'TestIngressDispatchesIntoARealHermesSession' ./cmd/server/
```
