INT-CI-015 — GitHub Actions Integration Tests (registry / PostgresStore) diagnosis + fix
=========================================================================================
Worktree: /home/kara/worktrees/crier-INT-CI-015  branch wt/INT-CI-015 (base dfe60a66 = origin/main at dispatch)

FAILURE (run 40318109287, job 121558530579, step "Run PostgresStore integration tests")
--------------------------------------------------------------------------------------
Full job log pulled via `gh api repos/crier-dev/crier/actions/jobs/121558530579/logs`
-> /tmp/ci-015-job-log.txt (4341 lines). Exit code 1 after 28.678s in
github.com/crier-dev/crier/internal/registry. 25 test failures, one family:

  - ERROR: relation "public.schema_migrations" does not exist (SQLSTATE 42P01)
    from golang-migrate's version probe (SELECT version, dirty FROM
    "public"."schema_migrations" LIMIT 1) and from newTestStore's cleanup TRUNCATE
  - ERROR: relation "agents" / "inbox_entries" does not exist (SQLSTATE 42P01)
  - ERROR: deadlock detected (SQLSTATE 40P01) x2 (TestPostgresOwnership_Transfer*)

ROOT CAUSE (verified, not inferred)
--------------------------------------------------------------------------------------
1. internal/registry/postgres_store_test.go TestMain starts ONE shared
   testcontainers Postgres for the whole package; testConnString is shared.
2. newTestStore (ibid. :103) runs `DROP TABLE IF EXISTS ... agents,
   schema_migrations CASCADE` against that shared DB before EVERY test.
3. Commit 79081696 (2026-10-25 05:30, "wip: INT-CI-014 checkpoint — adopt prior
   overlay-parallel batch, per-package ok") stamped t.Parallel() across the repo:
   9 integration-tagged files in internal/registry went 0 -> 93 t.Parallel()
   calls; internal/session integration files got 13 more (106 total).
4. Parallel tests DROP the shared schema out from under each other -> 42P01 +
   40P01. Local repro at dispatch HEAD: 50 failures, same error family
   (/tmp/ci-015-repro-1.log). Exact failing set varies per interleaving (25 vs 50),
   the mechanism is identical.

CI EVIDENCE WINDOW (gh api, exact)
--------------------------------------------------------------------------------------
  40180072531  2c232456  success  2026-10-25T09:18:09Z   <- last green
  40189293047  43f3d00c  failure  2026-10-25T11:32:23Z   <- first red
  40318109287  c27a952f  failure  (diagnosed run)
  40324125683  dfe60a66  failure  (current HEAD)
Every run since first-red failed; the only non-test change in internal/registry
in the window is a SQLite busy_timeout bump (untouched by these tests); every
diff line in the integration files is a t.Parallel() stamp.

Last-green integration job duration: 1m42s (09:25:37 -> 09:27:19) — the serial
regime is well inside the 5m budget; parallelism bought nothing here.

FIX (this branch)
--------------------------------------------------------------------------------------
1. Removed all 106 bare t.Parallel() stamps from integration-tagged files
   (12 files; python line-stripper, git diff = exactly 106 deletions):
     internal/registry/{postgres_store_test(62), migrate(9),
     postgres_ownership_integration(9), trust_postgres_integration(4),
     priority_integration(3), ttl_integration(2),
     ack_task_observation_integration(2), a2a_replay_integration(1),
     migration_portability_integration(1)}
     internal/session/{integration(11), request_idempotency_integration(1),
     task_idempotency_integration(1)}
2. NEW GUARD internal/registry/serial_suite_guard_test.go (NO build tag — runs in
   the default regime so the gitreins Tier-1 pre-commit gate and CI Build & Test
   execute it, and -tags=integration also runs it): scans every //go:build
   integration file in the module and FAILS on any bare `t.Parallel()` statement,
   naming file:line. Red-proofed twice (tagged and default regime): a planted
   stamp fails the guard with the exact file:line; clean tree passes.
3. internal/session/integration_test.go TestPostgres_FanoutDeliversThroughTheShippedInboxPath
   was failing for an UNRELATED, pre-existing reason (not gated anywhere — not in
   Makefile test-integration nor CI, which is why it survived): it expects 4
   fan-out outcomes including the message AUTHOR (atlas), but DF-CRIER-347
   (2dd9b44b, 2026-10-19) deliberately made "the author never a delivery target"
   (fanout.go withoutAuthor) and updated every gated test except this one.
   Test files were byte-identical to last-green before my strip (only stamps
   differed). Test-only correction to the shipped contract: expect 3 outcomes,
   nimbus+kappa deliveries, atlas inbox EMPTY (positive arm added), cited to
   DF-CRIER-347 in comments. No product code touched.

VERIFICATION (all from this worktree)
--------------------------------------------------------------------------------------
  go test -tags=integration -count=1 -timeout 5m ./internal/registry  -> ok 27.9s (was FAIL)
  go test -tags=integration -count=1 -timeout 5m ./internal/session   -> ok 12.5s (was FAIL)
  go build ./... ; go vet ./... ; go vet -tags=integration ./internal/{registry,session} -> OK
  go test ./... -count=1 -timeout 300s -p 4                            -> 30/30 packages ok
  make gofmt-check / shell-yaml-check / make-docker-check              -> PASS (616/61/11 files)
  Guard red-proof (default + integration regimes)                      -> FAIL-on-stamp proven

SCOPE NOTES
--------------------------------------------------------------------------------------
- Not touched: internal/permissions (integration-tagged, 0 stamps, no shared-DROP
  helper found), cmd/server a2a_*_integration files (0 stamps, no testcontainers).
- Did NOT broaden: dispatch_postgres_audit_test.go TRUNCATEs but is not
  integration-tagged and holds no stamps; migrate_test.go's own DROP helper is
  only reached serially now.
- Foreman to run gitreins guard + push; worker verified everything above locally.
