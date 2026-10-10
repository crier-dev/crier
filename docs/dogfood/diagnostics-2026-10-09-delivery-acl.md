# Dogfood diagnostics — 2026-10-09 delivery-ACL run

How this run was built, what broke, and the right way — so a later agent can
ask "does the ACL actually work" and get a real answer.

## Construction

- Built HEAD `8c316e3b` (`go build ./cmd/server`) into `/tmp/dogfood-crier/`
  — the scratch-dir rule: the run never writes inside the repo.
- Server armed with `CR_PERMISSIONS_ENABLED=true`, `CR_PERMISSIONS_DIR=/tmp/
  dogfood-crier/permissions`, `CR_PERMISSIONS_ADMIN_TOKEN=…`, port 18900 —
  a port chosen away from the defaults so a foreign listener cannot be
  mistaken for the test server (and this host HAS foreign crier processes:
  two crier binaries owned by uid 65532 and root were alive during the run;
  every HTTP claim here is backed by request_id-bearing access-log lines in
  server3/5/6.log written by the run's own process).
- The ACL store was left at 19 records: 5 principals, 5 bindings (incl. the
  two deliberate bad ones of DF-CRIER-301), 3 grants (one revoked tombstone,
  one expired), class records for atlas/bolt/chatter/quill/quill2.

## Errors encountered and what each proved

1. `Authorization: Bearer admin-token` → `401 invalid token`. Not a bug in
   isolation — but paired with the handler's 403 on the message token it
   proved the two-gate collision (DF-CRIER-297). A third data point: curl
   sending BOTH Authorization headers once → 403, because Go's
   Header.Get returns the first; that settled "maybe both headers work".
2. Zero timestamps on mint responses: every `created_at` in a 201 body was
   `0001-01-01T00:00:00Z` while the JSONL envelope `ts` was correct → the
   handler returns the record as built, not as stored (DF-CRIER-300).
3. `500 store failure` on role `superadmin`: the role enum is checked at
   record validation, so the client sees a 500 — an input error dressed as a
   server fault (DF-CRIER-301).
4. The 409 re-register trap: registering `atlas` with the wrong key first
   made atlas permanently unretrievable (signed retrieves 401). The run
   recovered by using a fresh agent id (`quill`) — exactly what a real
   operator would have to discover for themselves (DF-CRIER-302).
5. Two transient bunker spawn failures (`slice-limits: containment landing
   did not converge` on las-02; las-03 timed out entirely) → the skill's
   retry-once rule was applied, second spawn succeeded. Spawn-stage
   flakiness is real; a single failure is not evidence about the host.
6. `pkill -f "port 18901"` killed the harness command itself (self-match on
   argv) — the repo's own AGENTS.md load-safety section names this exact
   trap; `pkill -x crier` was used instead. Lesson kept: exact-name matching
   when the pattern appears in your own command line.

## What the numbers say (Step 2b, no PERF row filed)

Nothing a user would notice: ACL-checked delivery 8.5 ms ± 0.5 ms warm and
8.1 ms for a refusal, both dominated by the curl fork (server-side duration
field: 0.2–0.5 ms); boot-to-healthy with the ACL store armed 80–108 ms
(n=5, `/tmp/dogfood-crier/coldboot.sh`). The 10-01 run already established
the same profile for guard-attached delivery. Filing a PERF row for
microsecond server costs would be noise; the law is that unfelt wins are not
findings.

## Install leg (real, not substituted)

bunker-las-02, agent 1266c7c7 (spawn retried once after the transient
slice-limits failure), TTL 2h:
- clone https://github.com/crier-dev/crier.git as an uncredentialed fresh
  user — worked (public repo);
- bare Debian user → installed Go 1.26.0 by tarball (the README assumes a
  Go toolchain; a genuinely bare box needs this first — noted as friction,
  same class as the 09-25 run's finding);
- `make build` 67 s cold; `-version` → `v0.1.0-rc2-331-g8c316e3`;
- smoke: health 200, mint principal 201, register agent 201, class agent
  201 — the ACL arm path works from scratch;
- agent destroyed and verified absent from `bunker list`.

## Right way, for the next agent

- Use `examples/demo.sh` as the canonical client transcript; the deliver
  member is `payload`, retrieves are signed, acks need lease_id + ids.
- For ACL tests, class a control agent and LEAVE it unclassed — the
  legacy-posture control is what proves the ACL changed behavior rather
  than the port.
- Test grant expiry with a 2020 timestamp (inert grant) AND a revoke
  tombstone — they take different code paths and both must refuse.
- The capability-pool route is the T4 surface; point a pool at a
  cross-owner classed agent to see the resolution-then-check order.
