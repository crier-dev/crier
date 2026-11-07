# Changelog

All notable changes to crier are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project carries one version per release tag (`vMAJOR.MINOR.PATCH`,
plus a pre-release suffix such as `-rc1` while the project is pre-1.0).

Entries are written BEFORE the tag is cut, and the tag point is the commit that
contains them — a tag cut first can never contain its own changelog entry. The
procedure, the gate requirements and the publish step are in
[docs/releases.md](docs/releases.md).

## [Unreleased]

_Nothing yet._

## [0.4.0] - 2026-11-07

Range: `v0.3.0`..`4d0a6349` — 81 commits (13 merges and 68 non-merge: 1 `feat`,
7 `fix`, 9 `docs`, 8 `test`, 5 `chore`, 33 board, 3 foreman, 1 `qa`, 1 `pm`).
No `BREAKING CHANGE` footer and no `!` type token were found in the range, so
this is a minor release. The entries below group the material changes and name
the item id and commit they rest on; board and bookkeeping commits are excluded
from the notes but remain in the tagged tree.

### Added

- **Harness diagnostics explain missing opencode v2 model ids** (feat(harness), `86c4c469`).

### Changed

- **Ingress harness binding is explicit and reliable** (DF-CRIER-500, `e695ba2f`, `d31c6f27`).
- **Build information reports one deduplicated version identity** (DF-CRIER-502, `c7d8d534`).
- **Release publishing is idempotent** (RELEASE-012, `7860836f`).

### Fixed

- **Harness and integration test execution is deterministic** (INT-CI-027, `c4d3c240`; INT-CI-029, `8b458a99`).
- **Bunker isolation matrix is refreshed and its doc gate re-proved fail-closed** (INT-CI-020, `69e0601c`, docs `dda58953`, `5c93d9e2`).
- **The foreman's three self-inflicted dry-tick loops are stopped** (`9a3480ef`).

## [0.3.0] - 2026-11-06

Range: `v0.2.0`..`v0.3.0` — 433 non-merge commits (16 `feat`, 70 `fix`,
25 `docs`, 13 `test`, 2 `ci`, 2 `perf`, 142 `chore`, 120 board, plus
qa/dogfood/foreman/untyped). No `BREAKING CHANGE` footer and no `!` type
token anywhere in the range, so this is a minor release. The entries below
group those commits into user-visible changes rather than repeating every one:
each names the item id and the commit it rests on.

The entries above the `### Changed` split come from the range's last four days
(`2026-11-03`..`2026-11-06`) and were folded in from `[Unreleased]` by the cut
itself; everything below them was written for the range on 2026-11-03.

### Added

- **Ingress receipts are durable on the SQL session backends**
  (CRIER-INGRESS-4, `73fd5bb0`). `mode=inbox` now leaves a receipt row on SQLite
  and PostgreSQL as well as the JSONL store — route id, resolved agent,
  capability, inbox entry id, author, tags and `created_at` — so the row answers
  "where did this push land" on the same backends as the messages it describes.
  Storage is stated rather than assumed: the spec's abstract `chat_messages with
  kind=ingress.route` has no table here and could not use one (`chat_transcript`
  has `session_id NOT NULL`, and `mode=inbox` is the session-less mode), so the
  row is projected into its own `chat_ingress_routes` table, in the SAME database
  and view, with `kind` checked to the single legal value `ingress.route`.

- **The claude-code adapter, and one rule for harness names** (CRIER-HARNESS-9,
  -11, -12; `4cd76bfa`, `7e8fb878`, `a6e89626`). claude-code joins the adapter
  set on the same contract as the other four, with the surfaces read back from
  the installed 2.1.318 binary instead of guessed: `claude -p --output-format
  json` to create, `--resume <id>` to continue, the CLI-minted id bound to the
  Crier session on first delivery, and an unknown `--resume` answering
  `HARNESS_SESSION_UNKNOWN` rather than silently forking a transcript. Separately,
  `pi-agent` is now an ALIAS of `pi` — one implementation, one session table, one
  metric label — because our own installers published that name while the adapter
  id was `pi`, and a name that reaches no adapter is now refused at CONFIG LOAD
  with the accepted set printed, instead of surfacing at the first message.

- **A scheduled backup drill that proves its own restore** (CRIER-OPS-002,
  `379adbbf`). `scripts/backup-db.sh` and `scripts/restore-db.sh` were proven but
  unscheduled, so a missed day was silent: a job nobody scheduled cannot fail.
  `deploy/crier-backup.service` + `.timer` run the `--drill` arm — dump every
  configured store, verify each artifact, restore the PostgreSQL dump into a
  throwaway database and compare public-table counts against live.

- **The Hermes-session delivery transport** (`c53c1c51`). A message can now
  become one turn in a Hermes gateway session. This is a TRANSPORT over the
  gateway's existing `POST /api/sessions` and `POST /api/sessions/{id}/chat`,
  not a new protocol: per-agent policy `new|existing|per-agent` with
  `session_id`, `title` and `timeout_ms`; the credential is a mode-checked FILE
  path read at delivery time, so a rotation needs no re-registration and it is
  never logged or echoed in an error; redirects are refused so the bearer token
  is not replayed at another host; and the four failure paths answer four
  distinct named errors with their own statuses.

- **Outbound A2A client: crier can now ADDRESS a foreign A2A agent**
  (INT-A2A-007, `specs/A2A-OPTION.md` §5.7). New package `internal/a2a/client`
  plus the `crier a2a-client card|send|get|cancel` subcommand. It discovers the
  peer through `/.well-known/agent-card.json` and parses the card into the SAME
  `AgentCard` type the publisher projects, sends `message/send` / `tasks/get` /
  `tasks/cancel` with the server's own param structs (and its own PascalCase
  method names, with the pre-1.0 slash dialect selectable), and reports the
  peer's task id together with the SHA-256 digest of the card it addressed
  (`-record PATH` writes it). Idempotency is the wire's own: one `messageId` per
  send, reused across a retry, so a retry cannot create a second task on the
  peer. Credentials come from a file (mode-checked) or an environment variable
  only, are redacted under every `fmt` verb and JSON, and a card that requires a
  scheme the client cannot present is refused before anything is sent. Four
  failure paths answer four distinct named errors and exit codes — unreachable
  (1), no agent card / 404 (3), invalid JSON-RPC response (4), unauthenticated /
  401 (5) — and a 401 is exactly one request. **No route, flag or default
  changed:** `CR_A2A_ENABLED` keeps its meaning as the gate on crier's own A2A
  routes, and the client needs no switch at all.

- **Namespaced Prometheus series, served beside the legacy names for a dated
  dual-emit window** (MON-CRIER-005, `docs/observability-spec.md` §3.1).
  Every series now also emits under `crier_<subsystem>_<thing>_<unit>`
  (`deliveries_total` → `crier_delivery_accepted_total`,
  `http_requests_total` → `crier_http_requests_total`, … — the full 18-pair
  table is in the README §Observability). The twin is an ALIAS in
  `internal/metrics` rendered from the SAME series at scrape time, not a second
  counter: samples, help and type are identical by construction, no increment
  site changed, and a family renders once per scrape (a gauge callback is never
  evaluated twice, so the inbox queue is still read once and a failed depth read
  is still counted once). Legacy names are NOT removed here.
  `http_requests_total` also gained the bounded `route` label — the gorilla/mux
  path template, or the single literal `unmatched` when no route matched — on
  both names, so `sum by (route) (rate(crier_http_requests_total{code=~"5.."}[5m]))`
  returns a per-route error rate and `/metrics` scrapes are distinguishable from
  real traffic. **Dual-emit window opened 2026-11-04**; it closes only when
  `v0.4.0` has shipped, no dashboard/alert has referenced a legacy name for 30
  consecutive days, and a release scrapes zero legacy families — or on the
  operator's decision by 2027-05-04. The window was in fact closed the same day
  by MON-CRIER-007, which deleted the legacy names and the alias mechanism (see
  **Removed** below) before any deployed dashboard depended on the old names.

- **Named groups in the console, with measured rosters** (DF-CRIER-449,
  `daecd072`). The console creates through `POST /groups`, lists every group
  in the realm through `GET /groups` with a count MEASURED from the roster the
  server returned, reads one roster on demand through `GET /groups/{name}`
  (members plus the recorded actors), and its picker inserts the
  `@team:<name>` address into the composer. A named group is a curated roster
  that ONE message fans out to — every CURRENT member, through the same
  shipped fan-out path — and deliberately not a capability target: `@cap:y`
  stays the dynamic round-robin pool over live holders. Membership EDITS
  (`PATCH /groups/{name}/members`) remain API-only, and the panel says so
  instead of showing a control that cannot work.

- **Close and reopen a session, with the completion recorded as a durable
  actor event** (DF-CRIER-446, `c6f3e079`). `POST /sessions/{id}/state` closes
  and reopens a session; the `session.close` / `session.reopen` record retains
  through JSONL, SQLite and both PostgreSQL adapters, with state, `closed_at`,
  event and seq committing together, and the ordered `system_events` are
  served to the no-build REST UI. Illegal transitions are refused exactly, a
  stale close is refused, and the existing `SESSION_CLOSED` send diagnostic
  stays byte-identical. Historical SQL state-only rows receive no fabricated
  actor attribution.

- **Late-join context choices — ask before sharing thread history**
  (DF-CRIER-447, `37841743`). Adding a participant to a non-empty session
  prompts for how much of the thread they receive — summary or full, with a
  `context_boundary_message_id` marked `since` — and refuses a missing,
  unknown or cross-session boundary BEFORE the member is added; an empty
  session records none, with a nothing-to-share reason and no boundary. The
  stored applied choice is echoed, generated history is pinned to its
  inclusive message edge by seq, and server-authored event lines render in seq
  order: join-event text is never invented from a client request.

- **The depth rule in the browser** (DF-CRIER-448, `2efaec39`). Every message
  offers Reply in thread and Branch from here; a branch takes `branch:true`
  with a required, resolvable `parent_id` and becomes that child's own root
  (own `thread_id`, no `parent_id`), while the existing thread record keeps
  `parent_thread_id` plus `anchor_message_id`. Reply/branch links and the new
  branch rail open the exact `message-<id>` — revealing a containing collapsed
  group first, scrolling and focusing it — and mark the current message on
  navigation and on transcript scroll. A collapse-depth selector (1-6, default
  3 = `DEFAULT_THREAD_COLLAPSE_DEPTH`, matching the API's
  `DefaultCollapseDepth`) renders branches at or beyond the threshold as index
  cards stating exactly the count they withhold in the current filtered view;
  `thread_depth` stays the server's deliberate-branch depth, never
  `reply_depth`.

- **A UAT promotion gate that refuses dirty or mismatched provenance**
  (DF-CRIER-464, `f25f6268`). `scripts/deploy-ui-test.sh` probes the image's
  binary BEFORE removing the old container, pins promotion to that immutable
  image ID, and rejects unknown evidence printing the expected commit, the
  reported commit and the dirty flag. The replacement clones the existing UAT
  network aliases, persistent mounts, environment and restart/port settings,
  enforces the canonical `/data/sessions.db` path and a token read from an
  explicit file, verifies ownership and readiness, and requires three clean
  matching live `/version` reads; a failed live verification removes the
  unverified replacement, never its volume.

- **A board close-as-merged gate** (DF-CRIER-452, `1174d894`). The board gate
  that runs in the pre-commit hook and in CI now verifies, for every row a
  diff newly closes, that the recorded `commit_hash` is an ancestor of
  `main`/`origin/main`, and REFUSES the close when the hash is not an ancestor,
  when it resolves to no commit, or when the close prose claims the work was
  "already merged" while carrying no hash to prove it. It closed a real hole:
  three rows read complete for a day against a commit that existed only on a
  worker branch. Ordinary closes are unobstructed, rows already closed at HEAD
  are never re-judged, and the scope line names the bases and counts on every
  run.

### Removed

- **The 18 pre-rename Prometheus families, and the alias mechanism that served
  them** (MON-CRIER-007, `docs/observability-spec.md` §3.1) — **2026-11-04**.
  The MON-CRIER-005 dual-emit window is closed: `GET /metrics` serves the
  `crier_*` names only, and `internal/metrics` no longer has a second-name
  surface at all (the `Alias` method, its registry field and the render-time
  family rename are deleted), so a family can only be served under the name it
  was registered with. The retired names are `deliveries_total`,
  `guard_decisions_total`, `webhook_deliveries_total`,
  `capability_routed_total`, `capability_unheld_total`,
  `expired_messages_total`, `dead_lettered_messages_total`,
  `expiry_receipts_total`, `idempotent_replays_total`, `transfers_total`,
  `relay_events_total`, `http_requests_total`, `federation_held_current`,
  `ws_subscribers`, `inbox_queue_depth`, `inbox_queue_leased`,
  `inbox_queue_oldest_age_seconds` and `inbox_shed_total`; each one's
  replacement is named in the 18-pair table in README §Observability.
  **This is a breaking change for any dashboard, alert rule or query still
  keying on a legacy name** — repoint it at the `crier_*` name. The regression
  test `cmd/server/observability_namespace_test.go` renders the real default
  registry and fails if any of the 18 families comes back, so the window cannot
  silently reopen.

### Changed

- **SQLite session stores serve from a WAL journal** (PERF-001, `15c1d9d0`).
  `NewSQLiteStore` arms `PRAGMA journal_mode=WAL` on a file-backed database at
  open, before the schema is applied (the pragma returns a row, so it cannot ride
  `sqliteDialect.InitStatements`). The rollback journal is deliberately KEPT, and
  the reason logged at WARN rather than as a silent no-op, for `:memory:`, a DSN
  that cannot host the `-wal`/`-shm` pair, and any answer other than `wal`. The
  tests assert the pragma is IN FORCE, not merely issued.

- **The console's posture line states configuration, not read outcomes**
  (DF-CRIER-458, -459, -460, -461; `fd2b0dfb`, `db01477e`, `856dc286`,
  `6b780c49`). A configured posture is named rather than coloured red,
  same-cause refusals collapse, `enabled-unarmed` is no longer rendered as
  "ACL live; reads refused" over a populated panel, and a lifecycle control now
  states its authority requirement — an enabled button that cannot work was a
  lie.

- **The permission matrix is one cell per column** (`92b0ec0e`). A denied cell
  is `false`, never an absent key, so an unreadable column can no longer read
  as permission; the panel is proven in an authenticated browser session, not
  just a Node fetch.

- **The in-flight panel and watcher state only what the server said**
  (DF-CRIER-455, -456; `c2530bb7`, `8adbf50d`). Client constants and parse
  failures are no longer rendered as facts, and the watcher stops polling a
  hidden tab and says it is paused.

- **Group recipients are explicit** (DF-CRIER-487, -488, -489, -490;
  `c4c80287`, `fdc0e2fd`, `5c1ced6c`). Group recipients are named explicitly
  and the example IDs are gone, which prevents a stale extra delivery; an
  explicit named-group recipient is honoured instead of raising a false
  note-to-self warning; and a stale group response from another realm is
  ignored, keeping the active roster authoritative.

- **The chat page ships with a parse gate and a repeatable accessibility
  audit** (QA-CRIER-47, DF-CRIER-450; `46fae31a`, `e55ab446`, `eca0e64a`,
  `61a363d3`, `e7e1c457`). Merge-conflict markers that reached the tracked
  page now fail a committed JS parse gate rather than a visitor; the console's
  heading outline starts at `h2`, the three targets that fell under 24px are
  fixed, and the audit that fails on both is committed. Task lifecycle
  transitions render as human-readable lines instead of `{}` (DF-CRIER-491,
  `894db375`).

- **The documented surface is pinned to the served one** (DF-CRIER-492,
  DOC-2, DOC-3, DOC-4, DOC-5, DOC-6; `9b997644`, `4d6eae4c`, `cd47f34c`,
  `8b7edc88`, `496e671a`, `71786def`, `d629d638`, `5011d440`, `29fd5b64`).
  The README's API inventory sentence is pinned to the OpenAPI counts and a
  drift gate enforces it in `make` and in CI; the OpenAPI documents federation
  peer creation, federation patch and handshake, and the detection and
  containment routes; `CR_SERVICE_DELIVERY_AUDIENCE` is documented where an
  operator sets it; and the repository traceability doctrine is written down.

### Fixed

- **Every adapter config field is settable from the environment**
  (CRIER-HARNESS-15, `65914085`). `AdapterConfig.PermissionMode` had no
  `CR_HARNESS_*` entry, so the claude-code `--permission-mode` knob was YAML-only;
  and because the derived key upper-cased the adapter id, `claude-code` produced a
  key containing a hyphen, so NONE of the ten existing overrides resolved for that
  adapter either. The adapter segment is now rendered with hyphens mapped to
  underscores, so claude-code occupies `CR_HARNESS_CLAUDE_CODE_*`.

- **`/metrics` renders its series on an idle boot** (`6f2f2174`),
  `TestMetricsState_SeriesRenderOnIdleBoot` verified 10x. The Coverage job's
  other flake mechanisms are NOT claimed fixed here: they are tracked open as
  INT-CI-027 and QA-CRIER-56.

- **Membership writes require an authenticated, admin-cleared actor**
  (DF-CRIER-470, `a4c7f8ee`). `POST /sessions/{id}/participants` took the actor
  from the body, fell back to the unauthenticated `X-Agent-ID` header, and
  stored `actor: {}` when neither was present; `PUT .../context` did the same
  for `set_by`. Neither handler called `authorizeSender` and neither passed the
  `admin` gate a close/reopen enforces, so "member added by agent X" was a
  claim any caller could make. Both now resolve the actor as the message and
  state paths do, a signed caller must BE the declared actor
  (`403 SESSION_SENDER_IDENTITY_MISMATCH`), an actor-less write is refused
  `400 AUTHOR_REQUIRED` and writes nothing, and the assurance the request
  earned is server-stamped onto the record.

- **A member-add event names its actor, and a re-add is idempotent**
  (DF-CRIER-465, DF-CRIER-482; `9b5addf4`, `e0c54891`). A principal-created
  room recorded `actor: {}`; every member-add event now carries the actor, and
  re-adding an existing participant no longer forks the membership record.

- **`POST /sessions` is atomic** (DF-CRIER-481, DF-CRIER-471; `d0c2f75d`,
  `a8e3dd1f`). A later member's invalid context share rolls the whole create
  back or returns an error envelope, instead of leaving a partially created
  session; the atomicity is pinned by a dedicated test.

- **The context contract holds on every write path** (DF-CRIER-472;
  `ff9a4453`, `12604cef`, `f39bf6e2`, `f92f77cb`, `f7343158`, `e575f6c3`). The
  context boundary is validated for summary and full modes on all write paths;
  the context view respects the join instant and no longer inflates the given
  set; the GET error envelope is unified and divergence classified; `kind:team`
  is refused `400 UNKNOWN_TARGET_KIND`; an empty audience has an explicit
  outcome. The SQL view is applied in ONE transaction.

- **The console withholds management values by what they look like, not by
  what their key is called** (`60c91c01`). The previous guard was a field-name
  vocabulary, so a key pasted into `display_name`, a record carrying
  `apikey`/`auth`, or a raw 64-hex value under `digest` printed verbatim into a
  screenshot. The guard now inspects values and nested shapes — key prefixes
  (`sk-`, `ghp_`, `xox`, `AKIA`, `eyJ`, `PRIVATE KEY` envelopes), long hex and
  base64 blobs, record-level `{secret:true}` markers and credential-envelope
  shapes — and withholds them naming the REASON, never a sample of the value.
  Publication stays explicit: a 64-hex value under a `fingerprint`/`digest`
  name renders and is labelled "(published fingerprint)", and absent, empty and
  withheld are three distinct renderings.

- **Every ingress verdict value is retained** (DF-CRIER-457, `a04aab3c`).
  Conflicting or duplicate verdict headers can no longer make the wire and the
  audit disagree silently: all parsed values are retained, multiplicity is
  marked, a conflicting set leaves the verdict empty, and the completed audit
  records the final header values with their original key spellings (which
  `net/http` folds away at the receiver).

- **Chat state is distinguishable from missing state** (DF-CRIER-473, -474,
  -468, -485, -486; `d51d4ec3`, `3a40d939`, `6e71013c`, `ec581f96`,
  `6febe57a`). Absent and unknown chat state are different answers; session-wide
  events are marked outside thread filters instead of disappearing behind them;
  the agent-class read is disabled where the management credential cannot be
  held; group writes go to the PostgreSQL group store rather than local JSONL;
  and the identity suggestion is derived from the registry instead of being
  fabricated.

- **A task send and its progress panel cannot borrow each other's delivery**
  (DF-CRIER-467, `c10dfb3f`). A task send carries its own request key, and the
  progress panel never renders another message's delivery as its own.

- **The board and doctor gates check the thing they name** (DF-CRIER-453,
  -438, -439, -441; `bc679be8`, `34e72e22`). A board with zero nonblank records
  is refused, because byte size cannot prove population, and the refusal reports
  both file bytes and the record count. The doctor's probe now runs GIT's own
  entrypoints and its bound actually bites: a probe-only tell no longer earns a
  green, a missing `timeout` is exit 2, and a SIGTERM-trapping hook dies with
  the bound.

- **The tag publisher accepts a correct asset set** (RELEASE-010, `4a6eb500`).
  It uploaded `SHA256SUMS` with the assets and then compared the set to the
  manifest for EQUALITY — the manifest is not one of the files it covers,
  because a manifest that hashed itself could never be verified — so every
  future tag cut red on the one commit that must be green.

- **The QA batteries fail closed** (INT-CI-020, QA-CRIER-49; `6e82614b`,
  `5ed203e2`, `d830c5d6`, `ef8753e0`). A bunker agent spawned with an unenforced
  network-isolation mode now fails the cut instead of proving "no egress" from a
  flag value while running with full egress; an empty disconnect log is refused,
  because it hid a skipped suite; a live-test dependency failure under a network
  cut is classified UNVERIFIED rather than passing; and loopback is exempt from
  the dead proxy.

- **The CI suite is deterministic** (INT-CI-014, -015, -016, -017, QA-CRIER-45,
  -46, -48, QA-CRIER-52; `2193d793`, `dd9b96ef`, `ee6250a3`, `43f3d00c`,
  `ec9b760a`, `1176218f`, `ebd2c2b6`, `0974c733`, `c43582c9`, `b4b8ed54`,
  `3b2e7777`, `d269fab0`). The within-package timeout that stalled the suite is
  resolved and pinned by regression arms; the Postgres integration failure is
  diagnosed; the port-guard ARM E is deterministic and the selftest waits for
  its listener to bind; load-brittle wall-clock assertions are replaced with
  budget-relative bounds; `x/sys` is a direct dependency with a module-tidiness
  gate; and the pinned-boardctl and checkout steps carry the token they need.

### Notes

- **bunker-e2e is not a gate leg of this cut, and that is a recorded decision,
  not an omission.** Two earlier sweeps (`releng/v0.3.0-release-evidence.md`,
  RELEASE-009) held NO-GO because the exact-candidate bunker-e2e run stayed
  `pending` with zero jobs. It stays pending on every commit: the workflow's two
  `[self-hosted, bunker]` jobs have had no registered runner since the runner
  service was disabled on 2026-10-19, and the channel has produced ZERO
  successful runs in more than 100 attempts (INT-CI-019, open). A check that
  cannot reach a verdict on ANY commit is not a tree signal, and
  [docs/releases.md](docs/releases.md) does not list it among the gate
  requirements — those are `make build`, `make lint`, `make test-short`,
  `make release-artifacts`, and the full `.github/workflows/ci.yml` on the same
  commit, all of which are green on the tagged commit. INT-CI-019 remains the
  open defect and this cut does not claim a bunker-e2e verdict.

- **The tag publisher self-check that reddened v0.2.0 is fixed and selftested**
  (RELEASE-010, `4a6eb500`). That check uploaded `SHA256SUMS` and then compared
  the upload against a manifest that cannot name itself, so a CORRECT asset set
  failed after a complete upload. `make release-upload-selftest` now proves the
  exemption load-bearing on both implementations, with a NEUTER arm reproducing
  the old red. Its first proof on a real tag push is this tag's own workflow run.

- The range's remaining known defect is the Coverage job's intermittent red
  (INT-CI-027, QA-CRIER-56): it fails on commits whose diffs cannot reach the
  failing code, so it is test robustness rather than a regression, and the exact
  commit tagged here ran it GREEN (run `42242318816`).

- Every entry above names the item id and the commit it rests on; the range was
  counted from `git log --no-merges v0.2.0..HEAD`, not from a board summary.
  `[0.2.0]` carries a `Range:` preamble followed by a single `### Added`
  subsection; this entry keeps the preamble and the `### Notes` form
  `[0.1.0-rc1]` established, and splits the body into `### Added`,
  `### Removed`, `### Changed` and `### Fixed` — the Keep a Changelog names
  this file's own preamble declares — because this range's bulk is fixes, and
  carrying 70 fixes under `### Added` would misstate them.

- The two prior v0.3.0 readiness sweeps (RELEASE-009: `ec442a24` and the
  2026-10-30 event) recorded **NO-GO** on the bunker-e2e leg, and the evidence
  file `releng/v0.3.0-release-evidence.md` still says so. This cut supersedes
  that record for the reason in the first note above: the leg cannot reach a
  verdict on ANY commit, and `docs/releases.md` does not name it among the gate
  requirements. The gates measured at this tag point are `make build`,
  `make lint`, `make test-short`, `make release-artifacts` and full
  `.github/workflows/ci.yml` on the same commit.

- QA-CRIER-54: the clean-machine battery failed in the native suite because a
  shipped chat lifecycle test required Node.js, which a Go project does not
  provide. The Node-dependent tests now skip when Node is unavailable
  (`09348e91`).

- The structural promotion (`62564bcf`) moved the accumulated `[Unreleased]`
  content into this section and reopened an empty `[Unreleased]`. The cut
  re-dated the heading to the tag's own date (2026-11-06) and folded the
  `[Unreleased]` entries that had accumulated since 2026-11-03 back into the
  matching subsections above, so the tagged commit's section describes every
  commit the tag reaches.

## [0.2.0] - 2026-10-21

Range: `v0.1.0-rc2`..`v0.2.0` — 660 non-merge commits (75 `feat`, 121 `fix`,
43 `docs`, 12 `test`, 4 `ci`, 262 `chore`, 97 board, plus qa/dogfood/gitreins/
security/refactor). No `BREAKING CHANGE` footer and no `!` type token anywhere in
the range, so this is a minor release. The entries below are the release content
as it was written while the work landed.


### Added

- **Optional model/provider hints and a correlated reply on the dispatch
  envelope** (CR-FEAT-036, `docs/router-dispatch.md`). A lane dispatch may carry
  `model` and `provider` — the pair a consumer wants, or the members a bunker
  task-router interprets when one or both are default — plus `reply_to`, the
  Crier message id a returned result answers. Crier TRANSPORTS the two hints and
  never chooses one: an explicit value survives delivery byte for byte (no
  trimming, no validation against any catalogue of models), a blank string is
  preserved as sent, and `absent`/`null`/blank are all DEFAULT hints. Omission
  is the canonical spelling: absent/null become omitted members, while blank
  strings remain present and consumers must also treat them as default. No
  concrete model or provider is filled in. The members ride the request, the stored message
  (one JSONB column plus `reply_to`; migration 013) and every read of it —
  retrieve/lease, peek, list — so the polling router sees exactly what the
  scheduler wrote; a delivery that names none of them is byte-identical to
  before, and the remote-proxy deliver path forwards them verbatim instead of
  dropping them. Authorization is unchanged and stays out of the payload: the two
  sends a router deployment needs are two deliberate `send` grants (scheduler →
  the router's inbox, router → the scheduler's inbox), unrelated pairs are
  refused with `403 DELIVERY_FORBIDDEN`, and a `reply_to` — in the envelope or
  inside the payload — confers nothing. Covered by
  `internal/registry/crfeat036_test.go`: all four selection combinations across
  every supported spelling, over the real HTTP surface, plus the round trip
  (lease → correlated reply → ack), the signed-identity read gate, and the
  lease-expiry redelivery.

- **The session task lifecycle is now READABLE, not fire-and-forget**
  (DF-CRIER-304, `specs/CHAT-ADDRESSING.md` §2.5). Two read routes:
  `GET /sessions/{id}/tasks` lists every task in a session at its CURRENT state
  (one entry per task id, each with the task id the transitions take, the state,
  the owner once claimed, and the message record-version and thread the state
  lives in), and `GET /sessions/{id}/tasks/{task_id}` reads one — `404
  TASK_NOT_FOUND` for an id that is not a task in the session. The `POST
  /sessions/{id}/tasks` 201 response also names the task's OWN id and state at
  the TOP level (`task_id`, `state`) in addition to the existing nested `task`
  object: the message view's `id` is the MESSAGE id, so before this a caller
  reading the obvious top-level `state` saw null and a caller that claimed the
  top-level `id` was refused `TASK_NOT_FOUND` on a task it had created moments
  earlier. The current state is the latest record-version over the task id
  (§3.7 keep-LAST). Covered by `TestTaskLifecycle_Verifiable`, on both the JSONL
  log and the SQLite view.

- **A2A push-notification configuration as a view over crier's webhook config**
  (INT-A2A-005, `specs/A2A-OPTION.md` §5.5). The four `TaskPushNotificationConfig`
  operations of the A2A specification (`CreateTaskPushNotificationConfig`,
  `GetTaskPushNotificationConfig`, `ListTaskPushNotificationConfigs`,
  `DeleteTaskPushNotificationConfig`, §9.4.7) are served as methods of the
  existing `POST /a2a` binding — **no new route** — and they configure the push
  channel crier already has: the agent's webhook config. There is no second
  store, no second delivery engine and no second write path. A create or a delete
  is the body `PATCH /agents/{id}` already accepts, handed to that route's OWN
  handler, so crier's strict `webhook` member decode, its `webhook.Config`
  validation and its agent-owned signature gate apply to an A2A write exactly as
  they do to a direct one, and every field the A2A object has no member for
  (`retries`, `timeout_ms`, `batch`, `delivery_mode`, `schema_template`) is
  PRESERVED rather than reset. The configuration id is **derived**
  (`wh-<sha256(tenant‖url)>`), never stored, so there is no second source of
  truth to disagree with the row. **The capability answer is a MUST, not a
  nicety** (§3.3.4): an agent with no webhook — the same condition its Agent Card
  reports as `capabilities.pushNotifications: false` — gets
  `PushNotificationNotSupportedError` (`-32003`) from all four operations, never
  a silent success. `authentication.credentials` and `token` are refused rather
  than stored or dropped, because crier references a secret by NAME
  (`auth_value_ref`) and holds no credential to return; an inline
  `configuration.taskPushNotificationConfig` on a send stays refused, now naming
  the operations that do configure a channel. The notification itself is
  §4.3.3's `StreamResponse` envelope, `Content-Type: application/a2a+json`,
  delivered by the shipped webhook driver: the payload shape is the config's
  `custom_schema`, so the driver, its retries, its batching and its headers are
  untouched. Default-off and additive: with `CR_A2A_ENABLED` unset nothing here
  exists, no route or field was added, and a refused write leaves the row
  byte-identical. A2A remains an extra, not first-class support.

- **Capability-routed delivery — the registry's capability index as a dialable
  worker pool** (CR-FEAT-026). `GET /agents?capability=solver` could already tell
  you who advertises a capability, but delivery still required naming ONE agent
  id, so a pool of interchangeable workers could not be addressed as a pool (the
  gap the external review `DISPATCH · CRI-001` by Carter measured live at
  `ca28523d`). `POST /capabilities/{capability}/inbox` takes the same body as
  `POST /agents/{id}/inbox` and picks the holder: candidates are every agent
  whose advertised `capabilities` include the name (matched exactly like the
  discovery filter), **live holders rank first** (liveness is the registry's own
  derived status, so a crashed worker absorbs no work while a live one exists),
  and the choice **round-robins** over that pool — one holder per delivery,
  agent-id ascending, one cursor per capability, advanced once per dispatched
  delivery. The accept names what it chose (`"capability":"solver"`,
  `"target":"worker-b"`) because the sender has to know which worker took the
  work; a by-id delivery's body is unchanged. **Zero holders is a named error,
  never a silent drop**: `404 NO_CAPABLE_AGENT` naming the capability and stating
  that selection is local to the relay (federation forwards by agent id) — while
  a capability whose holders are all merely stale still ACCEPTS, because the
  inbox is durable and an untaken message is carried by the existing expiry
  receipt. A holder that dies **mid-lease** needs no case of its own: the lease
  expires and the message returns to the queue it was delivered to. A retry
  carrying an `idempotency_key` is scoped to the CAPABILITY (the holder is not
  known when the key is resolved), so a repeated key is answered with the first
  attempt's accept — same id, same `target` — instead of dispatching the same job
  to a second worker, and it is answered even if the pool has since emptied.
  Everything after the choice is the existing deliver path (guard choke point,
  webhook driver, durable inbox, lease/ack/TTL, federation fallback), and the
  detection layer observes a routed delivery with the RESOLVED holder as its
  target and a zero-holder refusal with an empty one. Two counters
  (`capability_routed_total`, `capability_unheld_total`) make a pool that has
  gone empty visible. Proved by `TestCapabilityDelivery*` in
  `internal/registry` (landing + signed retrieve + ack, the exact rotation, a
  stale holder skipped, the named refusal with nothing stored, capability-scoped
  idempotency, lease requeue after a holder dies, one observation per request,
  and a concurrent 40-delivery rotation under `-race`). Source: the external
  review `DISPATCH · CRI-001` by Carter, delivered via Bane.
- **Priority lanes and real backpressure** (CR-FEAT-035) — the three gaps the
  external review (`DISPATCH · CRI-001`, by Carter) named in passing, closed
  without changing what an existing deployment does:
  - **an optional `priority` on a delivery** (`POST /agents/{id}/inbox`,
    integer 0..9, default 0) — `GET /agents/{id}/inbox` hands back the highest
    priority claimable messages first, ties keep arrival order, and a delivery
    that names no priority is exactly the FIFO message it always was (the key is
    omitted from the wire at the default, so a pre-existing client parses
    identical bytes). Out of range is a 400 naming the range, never a clamp. The
    ordering is stored with the message (PostgreSQL migration 007, `priority
    NOT NULL DEFAULT 0`, range-checked), so it survives a restart and every
    message an existing database already holds reads back in the order it has
    always had.
  - **a global ingest budget** (`CR_RATE_LIMIT_GLOBAL_PER_MINUTE`, default 0 =
    no budget) — a shed on the delivery path itself, across every agent and
    sender, which the per-agent publish cap could never be: a flood from many
    ids spends every agent's own budget. A delivery over budget is refused **429
    `RATE_LIMITED_GLOBAL`** with a `Retry-After` header (whole seconds) and the
    same wait in the body, before any transport, guard call or store write —
    and the check sits after decode/validation/containment, so a malformed
    delivery still answers 400 and a contained agent still answers 403
    `AGENT_QUARANTINED`. The pre-existing **per-agent** publish 429 gained the
    `Retry-After` header the review found missing; its body and its scope are
    unchanged. Per-namespace budgets remain CR-FEAT-029's half (the shed counter
    is already labelled by scope, so that change adds series rather than
    renaming them).
  - **queue depth where operators already look** — `GET /status` reports
    `queue_depth` (`pending` / `leased` / `oldest_age_s`) beside
    `global_rate_limit_per_minute`, and `GET /metrics` exposes
    `inbox_queue_depth`, `inbox_queue_leased` and
    `inbox_queue_oldest_age_seconds` (gauges; `NaN` when the serving store
    cannot report a depth) plus `inbox_shed_total{scope}`. The store-wide
    numbers use the same predicates the per-agent `.../inbox/stats` counters
    already apply, so they are the sum of those, not a second definition of
    "queued".
  - Both 429 lanes now shed through one sliding-window implementation
    (`internal/ratelimit`), so "how long do I wait" has exactly one answer in
    the codebase.
- **Namespaces (realms)** (CR-FEAT-029) — a realm dimension on agents and
  messages, with per-realm policy. Named after the ancestor the external review
  pointed at — WAMP's realms, *"a routing and administrative domain … messages
  are only routed within a Realm"* — and opt-in: with `CR_NAMESPACES` unset
  every agent is in the one implicit namespace and every wire surface (agent
  rows, inbox entries, webhook envelopes, relay frames, `/relay/topics`) is
  byte-identical to a build without this feature.
  - **Per-realm policy** (`CR_NAMESPACES` inline or `CR_NAMESPACES_FILE`), each
    axis inherited when unset: auth posture (`shared` — the deployment's gate —
    or `token`, an additional realm credential resolved from an `env:VAR`
    reference and required on registration into the realm, delivery into it and
    relay publish/subscribe in it), **rate limits** (`rate_limit_per_minute`,
    counted per `(realm, agent)`, so one realm's flood cannot spend another's
    budget and the same agent id has one budget per realm), **guard settings**
    (`guard_enabled: false` keeps a realm's traffic out of the process-wide
    guard concurrency and circuit breaker — the noisy-neighbour valve — and
    `guard_policy` is the realm-level default policy) and **retention**
    (`retention_seconds`, the default message lifetime for the realm, which an
    explicit `ttl_seconds` still overrides).
  - **Realm-scoped routing with no implicit crossing**: a message's realm is the
    TARGET agent's stored realm, never the sender's claim — a delivery naming a
    different realm is `403 NAMESPACE_MISMATCH` and stores nothing; an inbox
    transfer between realms is refused; relay subscribers of the same topic name
    in two realms are two disjoint sets (wildcards included); a realm name the
    server does not declare is refused on every lane rather than falling back to
    the default realm; `PATCH /agents/{id}` refuses the member (moving a live
    agent is unregister + re-register).
  - **`GET /namespaces`** — the policies actually being enforced, with a live
    per-realm agent census, reporting postures and `env:` references and never a
    resolved secret. Not auth-exempt, like `/status`.
  - **Storage**: migration `008_add_namespaces` adds a nullable `namespace`
    column to `agents`, `inbox_entries` and `dead_letters`, where `NULL` is the
    default realm — an existing database gains three NULL columns and changes
    none of its answers. The webhook envelope and dead-letter records carry the
    realm too, so a message states it even after its agent's row is gone.
  - Spec: [`specs/NAMESPACES.md`](specs/NAMESPACES.md) (WAMP semantics read and
    cited, the `""`-canonical mapping, the decision table per lane, the
    non-regression table and the honest non-goals: the mesh lane is not
    realm-scoped, there is no cross-realm bridging, and a `token`-posture realm
    is not subscribable from a browser page without a header-capable client).
    Acceptance is measured live by `cmd/server/crfeat029_test.go` plus the relay
    and registry namespace suites.
  Source: external hands-on review `DISPATCH · CRI-001` by Carter (via Bane,
  2026-09-25).

- **Detection & containment** (CR-FEAT-030) — the other half of attribution, and
  it is opt-in (`CR_DETECT_ENABLED`, default `false`; with the flag unset no
  route is registered, no file is written and the delivery path is unchanged):
  - an **append-only, ed25519-signed delivery log** (`CR_DETECT_LOG`,
    `CR_DETECT_KEY`) — one record per delivery outcome (who sent what to whom,
    when, what the bus decided), hash-chained, fsynced, keyed from a file so it
    survives restarts, and **refused at startup** if a record was edited,
    deleted or signed by another key. `GET /delivery-log` pages the window and
    reports the true total; `GET /delivery-log/verify` re-reads the file and
    names the first record that does not verify.
  - **behaviour baselines with alerts** — `fanout_spike` (N distinct targets in
    a window), `new_peer_burst` (N first-ever conversations), `odd_hour_volume`
    (N messages inside the quiet window) and `canary_trip`, each one alert per
    agent per window, each with its thresholds and evidence at `GET /alerts`.
  - a **single-call kill-switch** (`POST /agents/{id}/kill-switch`) — pause the
    outbound webhook lane (dropping what was queued), release the leases the
    agent held, quarantine it (both directions refused with
    `403 AGENT_QUARANTINED`) and remove its registry row, with every action
    reported separately so a partial containment cannot read as a clean one.
  - **canary tokens** (`CR_CANARY_TOKENS`, or two generated per boot and served
    at `GET /canaries`) — a delivery addressed to a canary id, or carrying a
    canary token in its payload, trips `canary_trip`.
  - Acceptance, captured live against the real server wiring:
    `TestDetectionCatchesAndContainsACompromisedAgent` fans a scripted
    compromised agent out to five new peers, captures the alert, contains it in
    one call, proves the enforcement and then proves the log survives a restart
    with its chain intact. Design authority: `specs/DETECTION.md`. Source: the
    external review `DISPATCH · CRI-001` by Carter.
- **Task ownership: idempotency keys, expiry receipts, a dead-letter
  destination, and transfer/reassign** (CR-FEAT-025) — the four gaps the
  external review (`DISPATCH · CRI-001`, by Carter) named under "the lease is
  the lock", closed inside the existing lease model rather than beside it.
  `POST /agents/{id}/inbox` now takes an optional `idempotency_key`: a repeated
  key for the same agent inside `CR_IDEMPOTENCY_WINDOW_S` (default 24h) is
  answered with the original accept — same id, same status,
  `"idempotent_replay":true` — and stores nothing, so a sender that retried
  after losing the response no longer duplicates the target's work (a blocking
  webhook delivery replays the reply its single endpoint call produced; a
  rejected delivery records nothing, so a corrected retry is delivered rather
  than answered with a replay of the rejection). A message that expires
  unacknowledged now produces exactly one `MESSAGE_EXPIRED` receipt in its
  SENDER's inbox — the same durable error-notification shape as `WEBHOOK_FAILED`
  and `FEDERATION_FAILED`, which is why the sender is now stored WITH the
  message — and the message itself is preserved in a dead-letter destination,
  `GET /agents/{id}/inbox/dead-letters` (newest first, bounded by 7-day
  retention on a persisting backend and by capacity in memory, keyed by message
  id so a message is dead-lettered once and reported once, and NOT tied to the
  agent row so it survives the registration it was addressed to).
  `POST /agents/{id}/inbox/transfer` rebalances a stuck lease: messages move to
  another inbox UNLEASED and immediately claimable, under their own lease (409
  and nothing moved otherwise), with `force:true` as the explicit override for a
  holder that is gone. Migration `006_add_task_ownership` adds the two
  provenance columns to `inbox_entries` and the `dead_letters` table; the
  in-memory and PostgreSQL backends implement the same `PurgeReporter`,
  `DeadLetterStore` and `Transferrer` capabilities, and `PurgeExpired` keeps
  its count-only contract for any backend that does not.
- **`crier keygen`** (CR-FEAT-027) — the signing ceremony, replaced. The server
  binary now generates an ed25519 keypair in-process (no openssl, no xxd),
  writes it as a PKCS#8 PEM at mode `0600` in the same shape
  `openssl genpkey -algorithm ED25519` writes, and prints the agent config: the
  agent id, the hex public key, the exact `POST /agents` body and the next
  command. Before printing anything it re-reads the file, parses it with the
  server's own key loader and signs+verifies a sample payload, so the printed
  public key is provably the file's key; `-force` is required to overwrite an
  existing private key, and `-json` prints the same facts machine-readably. The
  external review (`DISPATCH · CRI-001`) named the openssl+xxd+sig()-helper
  ceremony as the number-one adoption killer, and it is what the last three
  testers each tripped over.
- **First-party clients** (CR-FEAT-027) — `clients/python/crier_client.py`
  (stdlib only, no packages to install) and `clients/typescript/crier.ts`
  (Node 22.6+, no packages either) expose the bus as methods: `register`,
  `deliver`, `retrieve`, `ack`, `stats`, `publish`, `subscribe`, plus
  `unregister` and a `request()` escape hatch. Both build the `X-Agent-ID` /
  `X-Agent-Ts` / `X-Agent-Sig` trio for you, decode inbox payloads, and surface
  the server's own error text. Runnable transcripts
  (`clients/python/round_trip.py`, `clients/typescript/round-trip.ts`,
  `clients/python/two_agents.py`) complete the round-trip and prove the
  signatures are enforced with negative controls — an unsigned call and a
  wrongly-signed call must both be refused. The Python signer prefers the
  `cryptography` package when present and otherwise uses a bundled RFC 8032
  implementation verified against the RFC 8032 §7.1 vectors and byte-identical
  to the library-backed path.
- **`make client-roundtrip-check`** (CR-FEAT-027) — the acceptance drive for the
  above: it builds the server, starts it on a port the shared port selector
  chooses, asserts it owns that port, runs `crier keygen`, both clients, both
  signing backends, the two-agent exchange and a second server with
  `CR_AUTH_TOKEN` (refusing an unauthenticated round-trip and accepting an
  authenticated one), and reaps the servers on exit. It also runs in CI.
- **The release front door: prebuilt binaries + a curl-to-install path**
  (CR-FEAT-028) — `gh release view v0.1.0-rc2` answered `assets: []`: the
  Release object existed and carried notes only, so the only way in was
  `git clone` plus a Go toolchain plus `make build`. An external hands-on review
  (`DISPATCH · CRI-001`, by Carter) filed that as a tester-funnel problem. Every
  release now carries 8 assets — `crier` and `crier-mcp` cross-compiled for
  linux/amd64, linux/arm64 and darwin/arm64, the installer, and a `SHA256SUMS`
  manifest. `scripts/release-artifacts.sh` (and `make release-artifacts`) builds
  them with `CGO_ENABLED=0 -trimpath`, stripped, stamping the same
  `internal/buildinfo` identity the other build paths stamp; it re-verifies the
  manifest and runs the host artifact's `-version` before returning, so a set
  whose identity did not land is never left on disk. `scripts/install.sh` is the
  one-line front door (`curl -fsSL …/install.sh | sh`): it detects the platform,
  verifies both binaries against that manifest and installs them into
  `$HOME/.local/bin`, and it REFUSES an unverified download — no checksum tool,
  no manifest entry for the binary, or a digest that does not match installs
  nothing, and there is deliberately no flag to skip the check. `make release`
  builds the asset set for the tag it cuts, and `make release-upload` attaches it
  to the Release object (creating it from the version's own changelog section and
  the compare link when it does not exist yet) — the publish step that used to be
  a hand-written, forgettable `gh release create`, which is how rc2 shipped
  without binaries. `make install-path-selftest` is the acceptance drive: it
  builds the set, serves it as a release tree, installs into a clean box whose
  `PATH` carries a `go` shim that exits 127 (so "no toolchain" is proven
  positively, not assumed from an absent `go`), runs the installed binary through
  `/health`, `/version`, a registration and an inbox delivery, and proves both
  unverified-download shapes are refused by name.

- **A2A interoperability, as an opt-in extra** (INT-A2A-001/002/003) — crier
  can be reached by an [A2A](https://a2a-protocol.org) client **without becoming
  an A2A-first system**: the whole option is behind `CR_A2A_ENABLED` (default
  `false`) *and* a per-agent `a2a` block, and with either half absent nothing
  A2A-related is registered — the route table, response bodies, auth
  requirements and storage schema are exactly what they were. The binding
  decision is recorded in `specs/A2A-OPTION.md` (JSON-RPC 2.0 over HTTP with SSE
  is implemented; gRPC and HTTP+JSON/REST are declared permitted-but-absent):
  - **`GET /.well-known/agent-card.json?agent_id=<id>`** serves the Agent Card
    projected from that agent's registry row — no card store, no cache, `404`
    for an id that is not an opted-in row, `400` when the request names no
    agent, and `Cache-Control: private` + a body-hashed `ETag` for conditional
    GETs.
  - **`POST /a2a`** is the JSON-RPC binding. `SendMessage` translates an A2A
    message into crier's **existing** delivery path — it calls
    `POST /agents/{id}/inbox`'s own handler, so the guard, sender idempotency
    (an A2A `messageId` becomes the deduplication key), the detection layer,
    federation hold/retry, webhook push, the durable inbox and the lease/ack
    lifecycle all apply unchanged — and answers a `Task` (accepted: inbox,
    queued push, or a federation hold with a status message that says so) or a
    direct `Message` (a target that answered inline). `SendStreamingMessage`
    answers `text/event-stream`, carrying the task's own inbox lifecycle
    (submitted → working → completed/failed, closing on the terminal state) and
    every event published to the task's relay topic — an SSE **adapter** over
    the existing relay subscription, with the WebSocket path untouched. The part
    model maps faithfully in both directions: A2A `text`/`raw`/`url`/`data` plus
    `metadata.alt`/`tags`/`caption` ⇄ crier message parts with `alt`/`tags`/
    `caption` and reference-or-inline file parts (a `raw` part gains the size and
    SHA-256 of its decoded bytes; a `url` part stays a reference), and a
    round-trip test proves a multi-part message survives to a crier consumer
    with `alt`/`tags` intact. Errors use the specification's own JSON-RPC codes
    and detail objects (`google.rpc.BadRequest` fieldViolations,
    `google.rpc.ErrorInfo`), and a refusal raised by the delivery engine carries
    crier's own status and body verbatim so a client can see exactly what crier
    said. What is deliberately NOT here: the task lifecycle operations
    (`GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`) and the extended
    card (INT-A2A-004/006) — each answers `-32601 MethodNotFoundError` naming
    the row that lands it rather than pretending. (The push-notification
    configuration operations are their own [Unreleased] entry above: INT-A2A-005
    added them as further methods of this same binding.)

- **A2A task lifecycle, mapped onto the inbox entry** (INT-A2A-004) — the four
  task operations, on the same opt-in `POST /a2a` binding: `GetTask` (the state
  of one task), `ListTasks` (one agent's tasks, filtered by `contextId`/`status`/
  `statusTimestampAfter` and cursor-paginated with `pageSize`/`pageToken`),
  `CancelTask` and `SubscribeToTask`.
  **There is no second store**: a task IS the inbox entry, its id IS crier's
  message id, and every state is read from that row's own lifecycle — an
  unclaimed message is `TASK_STATE_SUBMITTED`, a leased one
  `TASK_STATE_WORKING`, one past its TTL (or one the expiry sweep recorded in a
  dead letter) `TASK_STATE_FAILED`, and `CancelTask` — which releases the
  message's lease and closes it, whatever its lease state — answers
  `TASK_STATE_CANCELED`. Every task a read returns names the crier record its
  state came from (`metadata.crier.state_basis`), because the mapping is where
  an interop adapter most easily lies: there is no `TASK_STATE_COMPLETED` for an
  acknowledged task, since the ack REMOVES the entry and crier keeps no
  tombstone — such a task id answers the specification's own `TaskNotFoundError`
  ("invalid, expired, or already completed and purged") instead of an invented
  state, and the state mapping table is published in `specs/A2A-OPTION.md`
  §5.5.2. Refusals are refusals: a message sent to a task in a terminal state is
  `-32004 UnsupportedOperationError` (mandatory in §3.1.1, and nothing is
  delivered), a cancel of a terminal task is `-32002 TaskNotCancelableError`,
  and a message naming an OPEN task is `-32004` too, because crier cannot
  represent a continuation — a task is one inbox entry. The three store
  capabilities this needed are OPTIONAL and additive (a lease-free inbox
  listing, a per-entry close, and a single dead-letter lookup); no existing
  route, column, method or response body changed, and with the switch off every
  lifecycle method answers the router's `404` exactly as before. The
  push-notification configs and the extended card remain INT-A2A-005/006 and
  still answer `-32601`.

## [0.1.0-rc2] - 2026-09-21

### Added

- Release tooling (`RELEASE-001`): a `make release VERSION=vX.Y.Z` target that
  refuses anything but a clean `main` with a free `vX.Y.Z` tag, runs
  `make build && make lint && make test-short` and creates the annotated tag
  `crier VERSION` — printing the push commands for the configured remotes
  instead of pushing them; this changelog; and `docs/releases.md`.

## [0.1.0-rc1] - 2026-08-21

First release candidate — the feature-complete core carried by tag
`v0.1.0-rc1` (commit `aa96076`), packaged with an external-tester kit.

### Added

- **Relay (pub/sub)** — HTTP and WebSocket topic publish/subscribe with
  in-memory fan-out, a per-agent rate limiter keyed on `X-Agent-ID`, and
  at-most-once delivery to live subscribers (a publish to a topic with no live
  subscriber is accepted and dropped).
- **WebSocket mesh (peer-to-peer)** — agent-to-agent mesh connections with the
  framing, message types and correlation contract in
  [docs/mesh-protocol.md](docs/mesh-protocol.md).
- **Agent registry** — register, list and delete agents with their hex ed25519
  public keys and capability lists; agent-scoped endpoints require per-agent
  request signatures (`X-Agent-ID`, `X-Agent-Ts`, `X-Agent-Sig` over
  `METHOD\nPATH\nTS`) by default.
- **Durable inboxes** — deliver, signed retrieve and ack with a per-message
  `ttl_seconds` (24h default; `0` never expires and is reported as
  `"expires_at": null`), plus an optional PostgreSQL backend
  (`CR_DATABASE_URL`) so agents, their webhook/guard configuration and
  undelivered messages survive a server restart.
- **MCP bridge** (`cmd/crier-mcp`) — registry and inbox tools exposed over stdio
  MCP, in local and remote mode (`docs/specs/ci-007-mcp-server.md`).
- **Webhook delivery** — a target's configured webhook bypasses its inbox:
  blocking (`200`, the endpoint's reply returned to the sender) and queued
  `async`/`batch` (`202`) with retry, HMAC outbound signing, a dead-target
  circuit breaker, and exactly one durable `WEBHOOK_FAILED` in the sender's
  inbox when the retries are exhausted
  ([specs/WEBHOOK-DELIVERY.md](specs/WEBHOOK-DELIVERY.md)).
- **Message guard (LLM)** — prompt-injection classification at the delivery
  choke point (`CR_GUARD_ENABLED`, on by default): structured verdicts,
  fail-open with per-policy fail-closed, `X-Crier-Guard-*` response headers,
  provider failover and opt-in kanban cards
  ([specs/LLM-MESSAGE-GUARD.md](specs/LLM-MESSAGE-GUARD.md)).
- **Federation** — relay-to-relay links (`CR_FED_LINKS`) that forward a delivery
  for an unknown agent to each linked relay in order, hold and retry transient
  link failures for a named sender, share a link secret (`CR_FED_TOKEN`) and
  list peers on `GET /fed/peers` (CR-FEAT-006).
- **Single-source build identity** — `internal/buildinfo` stamped through
  `-ldflags`, surfaced identically by `./bin/crier -version`, `GET /version` and
  the startup log.
- **Observability** — opt-in Prometheus exposition on `GET /metrics`
  (`CR_ENABLE_METRICS`), the `GET /status` runtime posture document, and
  profiling.
- **PostgreSQL persistence** for the registry and inboxes (CI-003b).
- **Tester kit** — [TESTERS.md](TESTERS.md) (per-mode checklist), the
  docker-compose quickstart under `examples/agent-ecosystem`, and the committed
  E2E battery `scripts/e2e-battery.sh` (24 gates).
- **Review gates** — the gitreins Tier-1 pre-commit battery (secrets / build /
  lint / tests) plus the committed arms it does not cover: `gofmt` drift
  (DF-CRIER-189), tracked shell scripts and workflow YAML (DF-CRIER-206),
  Makefiles and Dockerfiles (DF-CRIER-209), the documented MCP launcher's stdout
  contract (DF-CRIER-137), and the prose claims executed by `make docs-check`
  (CR-GAP-055).

### Notes

- Gates measured at the tag point (`aa96076`): `go build`/`go vet` green, full
  suite 18/18 packages, gitreins Tier 1 PASS on both work commits, committed E2E
  battery 24/24 twice.
- v0.1.0-rc1 was cut by hand; `make release` (see [Unreleased](#unreleased))
  encodes that sequence as of the next cut.

[Unreleased]: https://github.com/crier-dev/crier/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/crier-dev/crier/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/crier-dev/crier/compare/v0.1.0-rc2...v0.2.0
[0.1.0-rc2]: https://github.com/crier-dev/crier/compare/v0.1.0-rc1...v0.1.0-rc2
[0.1.0-rc1]: https://github.com/crier-dev/crier/releases/tag/v0.1.0-rc1
