# Registry migration portability (DF-CRIER-366)

The registry keeps PostgreSQL's embedded SQL as the source sequence. SQLite
executes every up version, in order, in one transaction, selecting existing
`sqliteMigrationOverrides` where spelling translation is insufficient.
`registry_schema_migrations` records each applied version. Reopening does not
recreate the database or skip pending migrations.

## Census of every embedded migration

Every basename below has both `.up.sql` and `.down.sql` in
`internal/registry/migrations/` (17 pairs). "Raw portable" means equivalent
SQLite behavior without dialect adaptation, not merely accepting a type name.
SQLite accepts arbitrary declared type names such as JSONB, BYTEA and
TIMESTAMPTZ; that does not supply PostgreSQL JSON validation or timestamp/binary
semantics. SQLite has no PostgreSQL `~` operator or `jsonb_typeof` function.

| Basename | Raw up portable? / incompatibility | SQLite up handling | Raw down portable? / incompatibility |
| --- | --- | --- | --- |
| 001_create_agents | No: `::jsonb`, `octet_length`, `jsonb_typeof`, `btrim`, PostgreSQL types | Existing override: native types and array/key/status checks | Yes: DROP TABLE |
| 002_create_inbox_entries | No: GENERATED ALWAYS AS IDENTITY, `btrim`, PostgreSQL JSON/timestamp types | Existing override: AUTOINCREMENT, native timestamps/payload, lease checks | Yes: DROP TABLE |
| 003_add_agent_config_columns | No: multi-action ALTER, JSONB semantics | Existing override: separate ADD COLUMN statements | No: multi-action ALTER and DROP COLUMN IF EXISTS |
| 004_allow_keyless_agents | No: ALTER COLUMN / DROP and ADD CONSTRAINT, `octet_length` | Existing override: keyless constraint folded into 001; version still recorded | No: ALTER COLUMN / DROP and ADD CONSTRAINT |
| 005_add_agent_a2a_column | No equivalent JSONB validation/type semantics (parses as a type name) | Existing JSONB → TEXT translation | No: DROP COLUMN IF EXISTS |
| 006_add_task_ownership | No: `btrim`, JSONB/timestamp semantics | Existing type and btrim → trim translations | No: DROP COLUMN IF EXISTS (DROP TABLE itself is portable) |
| 007_add_inbox_priority | No: ADD CONSTRAINT | Existing override: inline priority range check | No: DROP CONSTRAINT and DROP COLUMN IF EXISTS |
| 008_add_namespaces | No: multi-action ALTER / ADD CONSTRAINT, `btrim` | Existing override: inline nonblank checks | No: multi-action ALTER / DROP CONSTRAINT / DROP COLUMN IF EXISTS |
| 009_add_inbox_thread_id | Yes: nullable TEXT column | Source unchanged | No: DROP COLUMN IF EXISTS |
| 010_add_inbox_location | No equivalent JSONB validation/type semantics (parses) | Existing JSONB → TEXT translation | No: DROP COLUMN IF EXISTS |
| 011_add_inbox_delivery_ms | Yes: BIGINT has SQLite integer affinity | Source unchanged | No: DROP COLUMN IF EXISTS |
| 012_add_inbox_principal_id | Yes: two separate TEXT-column ALTER statements | Source unchanged | No: DROP COLUMN IF EXISTS |
| 013_add_inbox_selection | No equivalent JSONB validation/type semantics (parses) | Existing JSONB → TEXT translation | No: DROP COLUMN IF EXISTS |
| 014_add_service_delivery_receipts | No: `~`, `now()`, BYTEA/timestamp semantics | Existing override: lowercase hex length/GLOB check, native types | Yes: DROP TABLE |
| 015_add_a2a_task_observations | No equivalent JSONB/timestamp semantics; no regex in this file | Existing JSONB → TEXT and TIMESTAMPTZ → INTEGER translations | Yes: DROP TABLE |
| 016_add_a2a_task_outputs | No equivalent JSONB/timestamp semantics | Existing JSONB → TEXT and TIMESTAMPTZ → INTEGER translations | Yes: DROP TABLE |
| 017_trust_tree | No: `~`, `jsonb_typeof`, `btrim`, JSONB/timestamp semantics | New override: lowercase hex checks, TEXT JSON validity + object check, integer timestamp | Yes: two DROP TABLE statements |

The SQLite runner is forward-only: down files remain PostgreSQL rollbacks and
are enumerated here, not secretly executed or claimed SQLite-compatible. This
change neither modifies PostgreSQL files nor changes earlier SQLite overrides.
Earlier translated JSONB columns are not newly upgraded to strict SQL JSON
constraints by this change; their existing Go write validation remains as-is.

## Chosen approach

Extend the runner's existing per-version SQLite override seam rather than add a
second filename convention/runner, weaken PostgreSQL constraints, or skip 017.
`sqliteMigrationDDL` is the shared selector used by startup and the executable
syntax guard. PostgreSQL still reads the original files byte-for-byte.

The 017 SQLite variant preserves nonblank ids/labels, NOT NULL, unique anchor
keys, signature primary key, the subject index, lowercase 64/128-hex shape and
JSON-object validation at the database boundary (not just the Go API). Explicit
NOT NULL compensates for SQLite rowid-table TEXT primary keys admitting NULL.
Hex checks include byte length and a NUL rejection because SQLite text length
and GLOB stop at embedded NUL. Records are TEXT with
`CHECK(json_valid(record) AND json_type(record) = 'object')`; signed timestamp
strings remain intact. There is no new dependency and no PostgreSQL DDL change.
Creating these tables does not add the optional SQLite TrustStore capability;
this task fixes migration/startup portability, not a new trust API backend.

## Regression and fail-proof

- `TestSQLiteMigrationChainSyntax` executes every selected embedded up migration
  against a fresh scratch SQLite file. Future migrations are discovered from the
  embedded filesystem, not a hardcoded latest-version list.
- `TestSQLiteMigrationChainSyntaxRejectsBrokenFixture` copies that complete chain
  and appends the next unused version containing a PostgreSQL `~` check. It must
  fail naming THAT fixture and the syntax error, not an earlier migration. The
  same fixture replaced by portable SQL must pass.
- `TestSQLiteFullChainRegisterReopen` uses the production constructor, asserts
  every migration version was recorded exactly once, registers/reads an agent,
  closes, reopens, and compares the complete agent and migration set.
- `TestSQLiteMigration017UpgradePreservesAgent` starts at a frozen version-16
  schema with an existing agent, then proves startup applies 017 without loss.
- `TestSQLiteMigration017Constraints` bypasses Go validators with direct SQL:
  invalid hex lengths/alphabet/case/NUL, non-object and malformed JSON, blank
  fields, NULL keys/record and duplicate public keys must fail; valid data passes.
- `TestPostgresMigration017Constraints` (integration tag) bypasses Go validators
  against real PostgreSQL to retain the equivalent existing constraints.
- `TestSQLiteRegistryProductionRestart` boots real `run()` in distinct OS
  processes, requires HTTP 200 `/health`, registers agents, and verifies their
  persisted identities and inbox lease after restart. Its transcript includes
  actual status/body responses and the durable SQLite startup marker.

Before the 017 override was added, the new tests failed with
`SQLite migration 017: SQL logic error: near "~": syntax error (1)`; the
production child exited 1 before `/health`. The standalone pre-fix cmd/server
binary reproduced the same error on a fresh database. The guard's appended bad
fixture is isolated in a test filesystem; no migration is added to production.
