# Dogfood diagnostics — 2026-10-16 (router hints + chat UI)

How the run was built, what broke, and the right way. Companion to
`2026-10-16-router-hints-chat-ui.md`.

## The probe stack (and why each layer exists)

- Scratch server on :18770 (`CR_SESSION_BACKEND=sqlite`, guard off, in-memory
  registry) — the fleet's live crier owns :8767; never probe it.
- `CR_DATABASE_URL=sqlite:///...` was tried first and failed with the postgres
  keyword/value parse error: the REGISTRY store is postgres-or-memory (known,
  DF-CRIER-316). The sqlite backend covers the SESSION store; in-memory
  registry is the documented demo posture and is what the router contract needs.
- Probe scripts were written with `crier keygen -json` + openssl for the pub
  key derivation; signing uses the TESTERS.md §2 recipe (`pkeyutl -sign -rawin`
  over `METHOD\nPATH\n<ts>` into a seekable file — a piped payload yields an
  empty signature and a misleading 401, DF-CRIER-2).

## Errors hit on the way (mine, not the product's)

1. **keygen flag memory failure** — invoked `keygen --agent-id/--agent/--out
   <dir>`; the real flags are `-id`, `-out <file>`, `-force`. `--help` first,
   every time (the exact pitfall the dogfood skill warns about). No finding:
   my guess was wrong, the tool's real interface is documented.
2. **Reply round-trip failed in run A** — the script extracted the dispatch id
   with a python one-liner that read a variable the shell had already dropped;
   `reply_to` went out empty and the scheduler-side match read ABSENT. Not a
   product defect (run A's own raw poll showed the API storing and echoing
   what it was given). Run C redid the round-trip properly: CORRELATION MATCH
   confirmed. Lesson: in a multi-step consumer probe, do the extraction in the
   same process that reads the poll.
3. **Cleanup deleted the keys before the unregisters** — run A's teardown
   `rm -rf $KD` ran before `DELETE /agents/...` which needs the key to sign.
   Both unregisters 401'd. Harmless here (scratch in-memory registry dies with
   the server) but the ordering is wrong in general: sign your cleanup, then
   delete secrets.
4. **Board append mangled by host writeback starvation** — the python append
   script read back a PREPENDED file (580 lines: my 4 rows at the top, 5 head
   rows gone, 1 glue line). Known failure class on this host (write() ok,
   read-back lies). Repair: snapshot `git show HEAD:<board>` to /tmp, rebuild
   as HEAD content + my rows appended, assert every HEAD line byte-exact and
   the final diff = 4 insertions / 0 deletions. `git diff` with deletion-count
   check is the only trustworthy read-back here.

## The UI run's false-signal lesson (run B v1 vs v2)

The first walkthrough never registered its agents — the UI is attribution-only
(its own error message says so). Result: recipient dropdown without the peer,
"presence unavailable", no delivery outcomes. That LOOKED like product bugs and
was none: the product requires registered identities for outcomes/presence, as
its disclaimers state. v2 registered both agents through the documented API
first, and every panel came alive. Before filing "the UI panel is empty",
drive the documented setup the page assumes.

## What was deliberately NOT exercised

- Webhook push envelope hints (out of scope by design — §8: hints travel on
  the inbox envelope; a push-configured target is a different transport).
- The sibling run's webhook failure ladder (their surface, rows 320-322).
- Delivery ACL granted/blocked arms for the reply (covered by the CR-CHAT-007
  run 10-09; this run's identities were unclassed agents on an ACL-less
  deployment, which is the router doc's own §5 recipe posture).
