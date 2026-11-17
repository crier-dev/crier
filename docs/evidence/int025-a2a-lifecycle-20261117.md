# INT-025 — A2A JSON-RPC SendMessage end-to-end: verification, not new code

Row: `INT-025` — *A2A JSON-RPC SendMessage end-to-end is unproven: registration and Agent Card
discovery work, no task-lifecycle turn was exercised through the protocol layer.*

Verdict: **the row's premise is stale.** The turn IS exercised, by two committed tests in
`cmd/server`. The row is closed on that evidence; no new code was written for it.

Tick: `crier-foreman-2026-11-17-11-52-37` · base `d21e6abc` (= `origin/main` at tick start).

## What the tree already contains

`cmd/server/a2a_lifecycle_test.go:232` — `TestA2ATaskLifecycle_Transcript` drives the JSON-RPC
binding on a real boot with `CR_A2A_ENABLED` and prints the whole lifecycle as a transcript.
`cmd/server/a2a_jsonrpc_test.go:273` — `TestA2AJSONRPCSend_ReachesACrierConsumerWithPartsIntact`
sends `SendMessage` and requires a **task id** in the answer (it fails if the binding answers with
anything else, which is the red-first shape the row asked for).

## Command and result

```
go test ./cmd/server -run 'TestA2AJSONRPCSend_ReachesACrierConsumerWithPartsIntact|TestA2ATaskLifecycle_Transcript' -v -count=1
=== RUN   TestA2AJSONRPCSend_ReachesACrierConsumerWithPartsIntact
=== RUN   TestA2ATaskLifecycle_Transcript
--- PASS: TestA2AJSONRPCSend_ReachesACrierConsumerWithPartsIntact (0.02s)
--- PASS: TestA2ATaskLifecycle_Transcript (1.22s)
PASS
ok  	github.com/crier-dev/crier/cmd/server	1.271s
rc=0
```

## The transcript (quoted from the test run, 15 steps)

```
deliver → stored, unclaimed        SendMessage/Task ffb3f371cfba20da048e2e9d = TASK_STATE_SUBMITTED
read → GetTask                     TASK_STATE_SUBMITTED, state_basis=inbox-entry-unleased
list → ListTasks                   1 task, TASK_STATE_SUBMITTED, totalSize=1, nextPageToken=''
retrieve (lease) → leased          GetTask = TASK_STATE_WORKING, state_basis=inbox-entry-leased
ack → entry removed, task purged   GetTask = -32001 TaskNotFoundError naming TASK_NOT_FOUND
ack → ListTasks                    0 tasks
TTL elapsed (unacked) → FAILED     GetTask = TASK_STATE_FAILED, state_basis=inbox-entry-ttl-elapsed-unacked
message → terminal task            REJECTED -32004 UnsupportedOperationError; queue_depth unchanged (0)
cancel → terminal task             REJECTED -32002 TaskNotCancelableError
cancel (leased task) → closed      CancelTask = TASK_STATE_CANCELED, state_basis=canceled-by-request
cancel → retrieve/ack              nothing claimable; the stale lease cannot ack (404)
cancel → read again + dup cancel   GetTask = -32001; duplicate CancelTask = -32001 TaskNotFoundError
message → open task                REJECTED -32004 (continuation not representable); queue_depth unchanged (1)
unknown task id → any op           -32001 TaskNotFoundError (never invented a state)
subscribe → terminal task          REJECTED -32004 UnsupportedOperationError (a JSON-RPC answer, not an empty stream)
```

The turn is observable end to end: `POST /a2a` `SendMessage` → a task id → `GetTask` states
`SUBMITTED` → `WORKING` → terminal states, with the inbox entry as the state basis.

## Residual (named, not skipped)

The row asked for the transcript "in docs/evidence". This file is that record; it is generated from
a live test run, not prose. If a future refactor removes the JSON-RPC binding's task-id answer, both
tests above go red — the pin is behavioural, not documentary.
