# Crier dogfood 2026-10-16 — router-dispatch hints (CR-FEAT-036) + rebuilt chat UI (CR-CHAT-038)

Tick: crier-dogfood-2026-10-16-04-24-10 · HEAD `839b4f2c` (binary `v0.1.0-rc2-409-g839b4f2`)
Sibling run same tick: `dogfood-2026-10-16-webhook-ladder` (rows DF-CRIER-320..322, commit `aa8ad5b`) — different surface, no overlap.

## Promise under test

"Crier is the transport between a scheduler that dispatches a lane and a consumer
that runs it: it carries optional model/provider selection hints untouched, hands
every dispatch back exactly as delivered, and carries the correlation a result
answers — and the rebuilt `/chat` page is a real operator's client of the public API."

## Angle

The last three dogfood runs (10-09, 10-10, 10-13) covered the ACL, chat sessions,
and federation peer-auth. This run takes the delta since `7c48745`: CR-FEAT-036
(model/provider hints + correlated router inbox delivery) and the CR-CHAT-038
chat.html rebuild — neither had ever been driven in real use.

## Part 1 — I am the bunker task-router (CR-FEAT-036, docs/router-dispatch.md followed verbatim)

Scratch server: in-memory registry + sqlite sessions, port 18770, guard off.

The consumer setup a real router does:

- `crier keygen -id <agent> -out <key> -force` — binary path, no openssl; the
  printed report re-verifies the key by signing with the server's own loader.
  `keygen -json` gives the registration config as one JSON object for scripts.
- Public key for registration: the human report prints it hex; a script derives
  it from the PEM (`openssl pkey -pubout -outform DER | tail -c 32 | xxd -p`).
  The `-json` output also carries it — this was the one small gap, see findings.

The contract, verified live (row **DF-CRIER-323**):

| arm | sent | read back |
| --- | --- | --- |
| explicit/explicit | `model:"glm-5.3-flash",provider:"zai-glm-default"` | byte-for-byte |
| default/explicit | `model:null,provider:"deepseek"` | model ABSENT, provider kept |
| explicit/default | `model:"gpt-6.1-sol"` | model kept, provider ABSENT |
| default/default | neither member | both ABSENT |
| blank strings | `"   "`,`""` | preserved as sent |
| non-string | `model:5` | 400 `invalid json`, nothing stored |

- Unsigned `GET /agents/{id}/inbox` → 401 exactly as the doc's replay shows.
- Signed poll returns `lease_id` + `queue_depth`/`leased_count`; ack under the
  lease → 204.
- Correlated reply: router delivers the result to the scheduler's inbox with
  `reply_to` = the dispatch's Crier message id; the scheduler's signed poll shows
  `reply_to` echoed on the stored message and joins the pair from its own inbox
  alone. CONFIRMED end-to-end.
- A forged claim (reply_to naming a conversation the sender did not own) is
  admitted identically to the honest shape — provenance, not authority, as §7 says.

**Verdict: the shipped behaviour matches docs/router-dispatch.md section for
section. Zero defects found on this surface.**

## Part 2 — the chat UI as two real operators (CR-CHAT-038 rebuild)

Two registered agents, two Chromium tabs (real browser, playwright +
google-chrome 152), full walkthrough (row **DF-CRIER-324**):

- identity apply → health line renders (`ok · online: 2/4`); channel create →
  header/meta/participants render; plain, addressed (per-recipient outcome
  `delivered`), and explicit-task sends all succeed with honest status lines.
- Recipient dropdown populates from the registry; the no-recipient task arm is
  refused client-side with the exact reason; auto-refresh (15s) and manual
  refresh both surface the counterparty's messages; reply-in-thread sets the
  thread filter; the Tasks panel jumps to the task thread; task state shows in
  the side panel (`open · Owner: unassigned`).
- Zero failed fetches (4xx/5xx) across both pages; 390px viewport has no
  horizontal overflow; cold open of `GET /chat` 159ms.

Cross-checked against the API after the UI run: message records carry
seq/thread/parent_id/outcomes exactly as the UI renders them.

One usability note recorded (row **DF-CRIER-325**, non-defect): a task message's
inline `task state: not exposed` is send-time text and never updates — the
side Tasks panel is the only live-state view. The page's own disclaimer covers
it; the cheap improvement would be linking the message card to the task thread.

## Part 3 — install leg (REAL, ephemeral bunker)

bunker-mvp was the only reachable bunker host (las-01..04 offline). Spawned
agent `crier-df-1016` (ttl 2h), git-archive-over-ssh from HEAD as the clone
substitute (private origin; no credentials minted — hard rule), then the
documented path verbatim: `make build` → **99s rc=0** on stock go1.22.2
(toolchain + modules auto-downloaded). Smoke: `-version`, health 200,
`GET /chat` 200, register 201, deliver 201, status 200 (build stamp matches
HEAD). Graceful `-stop` via pidfile. Destroyed; 95s-later residue recheck:
zero agents, zero homes orphans. Row **DF-CRIER-326**.

## Performance (Step 2b)

Headline ops, warm n=20 against the scratch server: `/chat` 0.4ms,
`/sessions` 1.0ms, transcript 1.0ms, deliver 201 0.4ms server-side
(11ms/op including curl fork). UI cold open 159ms. Nothing a user would
notice — **no PERF row**; this section is the record (same verdict class as
DF-CRIER-312/317/319).

## What a new integrator needs to know

1. Register identities with real keypairs (`keygen`), not the docs' abab… keys,
   before attempting a signed poll — the 401 names the missing headers, not the
   cause.
2. The inbox read is THE gated op. Everything else (deliver, register) is open.
3. `reply_to` joins on Crier's own message id — capture the `id` from the
   deliver response; it is not echoed anywhere else.
4. The chat UI needs the agents pre-registered (it is attribution, not
   authentication, by design) — a fresh viewer with no registry entries sees a
   healthy page with nobody to talk to.
5. `null` model/provider and absent members are the SAME default state on
   every read; only blank strings are distinguishable as "a value that is empty".
