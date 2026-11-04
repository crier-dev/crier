# crier — Observability Specification

Status: proposed (audit 2026-11-03, repo HEAD `104a2096`).
Scope: what crier exposes today, what an operator cannot see, and the concrete
metric contract that closes the gap. Written from source + live probes; every
claim below cites a `file:line` or a command that was actually run.

Related: `README.md` §Observability (README.md:2486), `DF-CRIER-142`
(the opt-in surface), `CR-FEAT-035` (inbox queue gauges), `docs/bunker-runbook.md`.

---

## 1. Role of the service, and what "healthy" means

crier is the fleet's agent-to-agent message bus: an HTTP + WebSocket relay
(`internal/relay`), a mesh of peer relays (`internal/mesh`), a durable agent
registry with inboxes/leases/dead-letters (`internal/registry`), a webhook push
driver with retry+backoff (`internal/webhook`), an LLM message guard
(`internal/guard`), federation hold-and-forward (`internal/federation`), and a
detection/containment side (`internal/detect`). One long-lived `cmd/server`
process serves everything on a single port (`CRIER_PORT`, default 8767 —
`deploy/crier.service` uses `EnvironmentFile=/etc/crier/crier.env`).

"Healthy" therefore means five things, in order of severity:

| # | Condition | Why it matters |
|---|-----------|----------------|
| H1 | Process is up and its port accepts connections | Every producer/consumer of the bus is blocked when it is not |
| H2 | The serving registry store answers (memory / sqlite / postgres) | `/status` itself grades `ok`/`degraded` on exactly this (`cmd/server/status.go` comment block, "health") |
| H3 | Messages flow: deliveries accepted, retrieved, acked; the inbox queue is not growing without draining | The whole purpose of the product |
| H4 | Nothing is silently lost: webhook dead-letters, TTL expiries, shed deliveries are visible and explained | Loss is the failure the bus must never hide |
| H5 | Guard and federation degradation is visible (provider circuit open, guard saturated, federation holding) | These fail *open* or *hold* by design, so silence is indistinguishable from health |

`GET /health` answers only H1 (`cmd/server/main.go:331`, always `{"status":"ok"}`).
`GET /status` (auth-required, `cmd/server/main.go:1014`) answers H1+H2 and reads
a queue snapshot on every call (`cmd/server/status.go:84-106`). Nothing today
gives a time series for H3–H5.

---

## 2. What exists today — verified

### 2.1 There IS a `/metrics` endpoint, and it is real Prometheus text

- `internal/metrics/metrics.go` is a **hand-written, dependency-free**
  implementation of the text exposition format v0.0.4
  (`internal/metrics/metrics.go:1-5`, `:29`). There is **no
  `prometheus/client_golang`** in `go.mod`/`go.sum` (verified: `grep -n
  prometheus go.mod go.sum` → no match). This is a deliberate choice recorded in
  the package comment, not an oversight — but see §2.4 for what it costs.
- Primitives: `Counter` (atomic float64 CAS, `:41-63`), `CounterVec` (`:72-103`),
  gauge-as-callback (`RegisterGaugeFunc`, `:226-230`), `Registry.render()`
  (`:251-273`). **No histogram and no summary type exists** — the format helper
  set has nothing to emit `_bucket`/`_sum`/`_count`.
- Handler: `Registry.Handler()` (`:277-284`) always returns `200` with
  `text/plain; version=0.0.4; charset=utf-8`. There is **no**
  gzip/OpenMetrics negotiation, **no** `process_*`/`go_*` runtime series, and
  **no** `promhttp.Handler()` instrumentation of the scrape itself.

### 2.2 It is wired, but only when `CR_ENABLE_METRICS=true`

- `cmd/server/observability.go:42-57` — `registerObservability` mounts
  `GET /metrics` **only if** `cfg.EnableMetrics`; otherwise the path falls
  through to the mux's 404 (`cmd/server/observability.go:22-23`).
- Config: `config/config.go:429-431` (`EnableMetrics` field),
  `config/config.go:1087-1092` (`CR_ENABLE_METRICS`, tolerant bool), default
  false (`config/config.go:430`).
- Wiring order matters and is correct: `registerObservability` is called after
  relay/mesh/federation exist because the gauges read those live objects
  (`cmd/server/main.go:1132-1150`).
- **Live probe (verified 2026-11-03):** a build of `cmd/server` at HEAD
  (`go build -o /tmp/obsprobe/crier ./cmd/server`) with
  `CR_ENABLE_METRICS=true CRIER_PORT=18767` served `GET /metrics` → **200** with
  18 `# HELP` families (full list in §3.1). With the flag unset, the same
  binary on `:18768` answered `GET /metrics` → **404** `{"error":"not found"}`.

### 2.3 Auth posture contradicts the documentation

- `internal/middleware/auth.go:139` — the exempt list is
  `/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs`. `/metrics`
  is **not** exempt; the code comment says so (`cmd/server/observability.go:24-26`).
- README says the same (README.md:1928, README.md:2494-2496): with
  `CR_AUTH_TOKEN` set, `/metrics` requires `Authorization: Bearer …`.
- **Live probe:** with `CR_ENABLE_METRICS=true CR_AUTH_TOKEN=probetoken` on
  `:18791`, `GET /metrics` without a header → **401**; with the correct bearer →
  **200**; `GET /health` without a token → **200**.
- Consequence for collection: a Prometheus scrape job pointed at crier needs the
  token in its config, or crier must be reached through a sidecar. This is fine
  as a decision, but it is currently *implicit* — nobody records that the
  fleet's scraper holds a crier bearer token.

### 2.4 The gaps that make it not-yet-a-Prometheus-endpoint

| # | Gap | Evidence | Consequence |
|---|-----|----------|-------------|
| G1 | **Off by default, and not set by the shipped unit** | `config/config.go:430`; `deploy/crier.service` sets no `CR_ENABLE_METRICS`; only `scripts/e2e-battery.sh:721` turns it on | Every deployed crier answering `404` on `/metrics` is *indistinguishable from a broken one*. This is the single biggest blind spot. |
| G2 | **Metric names are not namespaced** | `deliveries_total`, `http_requests_total` (`cmd/server/observability.go:35`), `ws_subscribers` (`:95`) | `http_requests_total` collides with `promhttp_metric_handler_requests_total` conventions and with any other product's series in a shared Prometheus. The convention required by this audit is `<product>_<subsystem>_<thing>_<unit>`. Migration needs a **dated rename + a dual-emit window**, since live dashboards may already key on these names. |
| G3 | **No latency signal anywhere** | No histogram type exists in `internal/metrics/metrics.go`; the only duration recorded is a **log line attribute** (`internal/middleware/middleware.go:40`) | "How long does a delivery take?" is answerable only by grepping `crier.log`. No SLO, no p99, no alerting on slow webhook drives. |
| G4 | **No runtime self-series** | `go.mod` has no client_golang; nothing emits goroutines, GC, heap | Cannot answer "is it leaking goroutines/connections" from the metrics surface (pprof answers it only if `CR_ENABLE_PPROF=true`, also default off). |
| G5 | **No webhook queue depth or retry lag** | `internal/webhook/driver.go` counts only *terminal outcomes* (`:25`, incremented at outcome sites); retry/backoff state lives in the driver's queue and is only **logged** (`:403`, `webhook: … retry` log lines) | An endpoint that is degraded, not dead, shows `failed` climbing with no way to see *how much is waiting* or *how old the oldest unresolved item is*. |
| G6 | **No guard latency, saturation, or circuit state** | `internal/guard/router.go:104,142` hold `sem chan struct{}` and a per-endpoint `*Circuit`; `ErrConcurrencySaturated` (`internal/guard/router.go:166`) and circuit-open (`:287`) are returned but never counted | The guard fails open on saturation; an operator sees deliveries succeed with no guard metadata and no signal why. |
| G7 | **No extraction/detection signal** | `internal/detect/detect.go:563` `Alerts()` returns a grow-only in-memory slice (`:537` appends) | Alerts and quarantines are pullable via `GET /alerts` and never trended; an alert storm is invisible as a rate. |
| G8 | **No store-health/size signal** | `/status` grades degraded but emits no counter; `inbox_queue_*` gauges go `NaN` on read failure (`cmd/server/observability.go:125-128`) | `NaN` is honest, but a store that is *intermittently* unreadable produces gaps a rate can't explain, and there is no counter of failed depth reads. |
| G9 | **`/metrics` is a whole-request-path blind spot for its own cost** | `countRequests` wraps the mux only when metrics are on (`cmd/server/observability.go:45-47`); the request counter labels by **status only** (`:35-36`) | You cannot separate `/metrics` scrapes from real traffic, and cannot see which route is slow (path is a log attribute only). |
| G10 | **No federation retry/lag curve** | `HoldManager.Pending()` is the only exposed value (`cmd/server/observability.go:87-94`) | "Holding 40 messages for 20 minutes" looks the same as "holding 40 for 20 seconds". |

Also worth stating plainly: the package comment in
`internal/metrics/metrics.go:1-5` claims the design is complete. It is complete
for *counters and gauges*. It has no way to express a timing distribution, which
is exactly what G3/G6/G7 need — so the honest fix is either extend this package
with a small fixed-bucket histogram, or adopt `client_golang` + `promhttp`. The
spec below is written to be implementable **either way**; the naming and
semantics do not depend on the library.

---

## 3. Metric catalogue

Naming convention: `crier_<subsystem>_<thing>_<unit>`. Existing series are
listed in §3.1 as they are **today**, with their proposed names in the last
column; §3.2 is the new surface. Low-cardinality only: no agent id, message id,
request id, path, prompt, URL or timestamp in any label. Bounded label domains
are stated per metric.

### 3.1 Existing series (18) — today's names, proposed rename

All of these are registered today; names/paths verified by live scrape and by
the registration sites cited.

| Today | Type | Site | Proposed name |
|-------|------|------|---------------|
| `deliveries_total` | counter | `internal/registry/handler.go:37`, inc at `internal/registry/dispatch.go:419,465,566` | `crier_delivery_accepted_total` |
| `guard_decisions_total{decision}` | counter | `internal/registry/handler.go:43`, inc `internal/registry/dispatch.go:312,321,328` | `crier_guard_decisions_total{decision}` |
| `webhook_deliveries_total{outcome}` | counter | `internal/webhook/driver.go:25`, inc at `:248,387,397,401,503,509,539,547,558,564,962,988,996,1001` and `internal/webhook/pause.go:67` | `crier_webhook_deliveries_total{outcome}` |
| `capability_routed_total` | counter | `internal/registry/capability.go:99`, inc `:255` | `crier_capability_routed_total` |
| `capability_unheld_total` | counter | `internal/registry/capability.go:105`, inc `:242` | `crier_capability_unheld_total` |
| `expired_messages_total` | counter | `internal/registry/ownership.go:35`, inc `:107` | `crier_inbox_expired_total` |
| `dead_lettered_messages_total` | counter | `internal/registry/ownership.go:37`, inc `:143` | `crier_inbox_deadlettered_total` |
| `expiry_receipts_total` | counter | `internal/registry/ownership.go:39`, inc `:173` | `crier_inbox_expiry_receipts_total` |
| `idempotent_replays_total` | counter | `internal/registry/ownership.go:41`, inc `internal/registry/handler.go:1284` | `crier_delivery_idempotent_replay_total` |
| `transfers_total` | counter | `internal/registry/ownership.go:43`, inc `:333` | `crier_inbox_transfers_total` |
| `relay_events_total` | counter | `internal/relay/handler.go:26`, inc `:266` | `crier_relay_events_total` |
| `http_requests_total{code}` | counter | `cmd/server/observability.go:35`, inc `:168` | `crier_http_requests_total{code,route}` |
| `federation_held_current` | gauge | `cmd/server/observability.go:87` | `crier_federation_held_current` |
| `ws_subscribers` | gauge | `cmd/server/observability.go:95` (relay `internal/relay/relay.go:423` + mesh `internal/mesh/peer.go:367`, summed) | `crier_ws_subscribers` |
| `inbox_queue_depth` | gauge | `cmd/server/observability.go:133` | `crier_inbox_queue_depth` |
| `inbox_queue_leased` | gauge | `cmd/server/observability.go:136` | `crier_inbox_queue_leased` |
| `inbox_queue_oldest_age_seconds` | gauge | `cmd/server/observability.go:139` | `crier_inbox_queue_oldest_age_seconds` |
| `inbox_shed_total{scope}` | counter | `internal/registry/backpressure.go:77`, inc `:133` | `crier_inbox_shed_total{scope}` |

Note on `inbox_queue_*`: these read the serving store per scrape
(`internal/registry/sqlite_store.go:542-552` runs one aggregate `SELECT` over
`inbox_entries`). At scale that is a full-ish scan on every 15s scrape — see
row §5 R4 (caching / `cache=true` decision).

### 3.2 New series — minimal viable set (row R1 in §5)

These four answer "is the bus up, is it doing work, is anything queued or
failing" and are the R1 deliverable. They need no new library: gauges and
counters already exist in `internal/metrics`.

| Name | Type | Unit | Labels | Question it answers |
|------|------|------|--------|---------------------|
| `crier_up` | gauge (const 1, registered at boot) | — | — | Can Prometheus distinguish "process gone" from "process up but scrape failed"? (Absent series = down; `up==1` = alive.) |
| `crier_build_info` | gauge (const 1) | — | `version`, `commit`, `dirty` — all **build-time** constants from `internal/buildinfo`, already served by `GET /version` | Which build is this target running? (Standard `*_build_info` pattern; version strings are bounded per release, not per request.) |
| `crier_store_healthy` | gauge 0/1 | — | `backend` ∈ {memory, sqlite, postgres} (`cmd/server/status.go` enum) | Is the serving store answering right now (H2)? |
| `crier_start_time_seconds` | gauge | seconds (unix) | — | Restart detection and uptime = `time() - crier_start_time_seconds`. |

Why so few: the existing 18 series already cover freshness/volume for
deliveries, shed, expiry and dead-letters once the endpoint is *reachable*.
G1 (off by default) and the four above are worth more than twenty new series.

### 3.3 New series — depth (rows R2–R4)

**Latency (R2).** Requires a histogram. Buckets fixed: `{0.005,0.01,0.025,0.05,
0.1,0.25,0.5,1,2.5,5,10,30}` seconds — a message bus with a 30s webhook timeout
(`webhook delivery timeout=30s`, boot log) does not need finer than 5ms nor
coarser than 30s.

| Name | Type | Unit | Labels | Question |
|------|------|------|--------|----------|
| `crier_http_request_duration_seconds` | histogram | seconds | `route` (mux template, e.g. `/agents/{id}/inbox` — **bounded, ~66 templates**, `docs/openapi.yaml`), `method`, `code` | Which route is slow, and is the p99 rising? Replaces the log-only duration at `internal/middleware/middleware.go:40`. |
| `crier_webhook_delivery_duration_seconds` | histogram | seconds | `outcome` (delivered/failed/deadlettered/dropped) | How long does a push take to reach a terminal state, including retries? |
| `crier_guard_check_duration_seconds` | histogram | seconds | `decision` (allow/block/sanitize), `errored` (true/false) — `internal/guard/guard.go:417` already computes this pair | Is the LLM guard slowing ingest or failing open? |
| `crier_inbox_deliver_duration_seconds` | histogram | seconds | `transport` ∈ {inbox, webhook} | Store-write vs webhook-accept cost split. |

**Guard health (R3).** These close G6. The state already exists; nothing emits it.

| Name | Type | Unit | Labels | Question | State source |
|------|------|------|--------|----------|--------------|
| `crier_guard_provider_circuit_open` | gauge 0/1 | — | `provider` (`spec.Provider`, a bounded preset enum: deepseek/openai/… — `internal/guard/router.go:249`) | Is a provider's circuit breaker currently open? | `internal/guard/circuit.go` `openedAt` map, read via a new `Circuit.Open(key) bool` accessor |
| `crier_guard_inflight` | gauge | checks | — | Are we at the concurrency cap (default 8, `CR_GUARD_MAX_CONCURRENT`, `internal/guard/router.go:114-115`)? | `len(r.sem)` on the `sem chan struct{}` (`internal/guard/router.go:142`) |
| `crier_guard_saturated_total` | counter | checks | — | How often did a check bounce with `ErrConcurrencySaturated` (`internal/guard/router.go:166`)? | increment at that return |
| `crier_guard_all_providers_failed_total` | counter | checks | — | How often did the whole fallback chain fail (`ErrAllProvidersFailed`, `internal/guard/router.go:353`)? | increment at that return |

**Webhook queue (R4).** Closes G5.

| Name | Type | Unit | Labels | Question |
|------|------|------|--------|----------|
| `crier_webhook_queue_depth` | gauge | items | — | How much outbound push work is waiting? (Needs a `Driver` accessor over its queue.) |
| `crier_webhook_queue_oldest_age_seconds` | gauge | seconds | — | How stale is the oldest un-delivered item — is the endpoint down or just slow? |
| `crier_webhook_attempts_total` | counter | attempts | `attempt` (1..`retries`+1, bounded by config, default 6) | What is the retry rate as a *rate*, rather than per-line log grep? |

**Federation and retention (R4).**

| Name | Type | Unit | Labels | Question |
|------|------|------|--------|----------|
| `crier_federation_held_age_seconds` | gauge | seconds | — | How long has the oldest held federated delivery waited? (Needs a `HoldManager` oldest-item accessor; today only `Pending()` exists.) |
| `crier_store_queue_depth_read_errors_total` | counter | reads | `backend` | Is the store intermittently failing to answer (explains `NaN` gaps, G8)? |
| `crier_detect_alerts_total` | counter | alerts | `signal`, `severity` — both bounded enums from `internal/detect/detect.go:118-125` | Is an alert storm happening? (`Detector.Alerts()`, `internal/detect/detect.go:563`, is grow-only) |
| `crier_detect_quarantined_current` | gauge | agents | — | How many agents are currently contained (`internal/detect/detect.go:52` `Containment`)? |

**Process runtime (R4, optional).** If `client_golang` is adopted, `go_*` and
`process_*` come for free; if the hand-written registry stays, at minimum emit
`crier_go_goroutines` and `crier_process_open_fds` from
`runtime.NumGoroutine()` and `/proc/self/fd`. Readiness with the mesh plus
long-poll inbox readers holding many descriptors (`LimitNOFILE=65536`,
`deploy/crier.service`) makes FD exhaustion a real 3am question.

### 3.4 Deliberately NOT instrumented

- **Per-agent, per-message, per-request-id, per-path labels.** `agent` id would
  be unbounded (agents are registered dynamically at `POST /agents`); message
  ids are unbounded by definition. `/agents` roster size is instead available
  from `/status` (`agents_online`/`agents_total`), which is the right place for
  a per-read projection.
- **Payload content, guard prompts, webhook URL query strings.** Already
  excluded from logs (`internal/webhook/driver.go` `dispatchArgs` comment,
  "never the query — it may carry a credential") and must stay out of labels.
- **`CR_AUTH_TOKEN` / DB connection strings.** `/status` already proves the
  discipline of never serializing these (`cmd/server/status.go:33-36`).
- **Auth/signature timings.** A timing side-channel is not worth a label.
- **Per-topic relay label.** Topics are caller-chosen strings; use
  `crier_ws_subscribers` (already summed, `cmd/server/observability.go:95`) and
  leave per-topic breakdown to logs.

---

## 4. Exposure and collection path

### 4.1 Exposure

Serve the series on the **existing HTTP server** — there is exactly one listen
socket (`cmd/server/main.go:1157-1163`, `Addr: :<cfg.Port>`) and `/metrics` is
already mounted on that mux (`cmd/server/observability.go:46`). No sidecar, no
second port. This is already the right mechanism; what is missing is the flag
being on and the metric set being complete.

Two decisions to make explicitly, both recorded as rows:

1. **Default.** Keep `CR_ENABLE_METRICS` defaulting to false (defensible:
   `README.md:1928`), but make the shipped unit set it:
   `deploy/crier.service` and `deploy/crier.user.service` should gain
   `Environment=CR_ENABLE_METRICS=true` (or the deployment's
   `/etc/crier/crier.env` should). A product whose metrics are opt-in *and*
   unconfigured in its own unit is the G1 failure. **Recommendation:** default
   the flag to true (metrics endpoints disclose no secret — the series list is
   static and documented in README.md:2490) and let an operator turn it off.
   _Landed in MON-CRIER-001: the default is now true (`config/config.go`
   `LoadFrom`), both units carry `Environment=CR_ENABLE_METRICS=true`, and
   `CR_ENABLE_METRICS=false` remains the operator override._
2. **Auth.** Keep `/metrics` non-exempt (auth-off servers stay open; auth-on
   servers require the bearer). Then the scrape job MUST carry the token. If
   that is unacceptable operationally, the alternative is to add `/metrics` to
   the exempt list at `internal/middleware/auth.go:139` **only** when a new
   `CR_METRICS_PUBLIC=true` is set — an explicit operator choice, not a default.

If `client_golang` is adopted (recommended for the histogram work), keep the
existing `internal/metrics` registry as a thin adapter so no call site changes:
`NewCounter` → `prometheus.NewCounter`, `RegisterGaugeFunc` →
`prometheus.NewGaugeFunc`, `Handler()` → `promhttp.Handler()`.

### 4.2 Collection

Fleet Prometheus 3.15 lives on `fleet-monitor`; agents **push** via
remote_write, and anything scrape-only needs a scrape job in
`/root/fleet-monitoring/prometheus/prometheus.yml`. For crier:

```yaml
- job_name: crier
  scrape_interval: 15s
  scrape_timeout: 10s
  metrics_path: /metrics
  authorization:
    credentials_file: /root/fleet-monitoring/secrets/crier-token   # CR_AUTH_TOKEN value
  static_configs:
    - targets: ['<crier-host>:8767']
      labels:
        host: <crier-host>
        product: crier
        service: crier-bus
```

**Shipped example.** `deploy/prometheus-scrape.example.yml` is the job above as
a paste-in, including the `authorization.credentials_file` line the
non-exempt route requires (the file holds the `CR_AUTH_TOKEN` value; the token
never appears in the Prometheus config). With MON-CRIER-001 landed this is the
whole collection path — the server registers `/metrics` on a bare boot
(`CR_ENABLE_METRICS` defaults to true), both shipped units set it explicitly,
and the identity/health series of §3.2 (`crier_up`, `crier_build_info`,
`crier_store_healthy`, `crier_start_time_seconds`) are registered at boot, so
the only silence left is a `401`/`up=0` an operator can alert on.

Caveats an implementer must respect (they are why this section is not just a
paste-in):

- **If the token is not set up**, a config with `authorization` against a
  server running with auth *off* is harmless; against auth *on* without the
  header it yields `401` and `up=0` — which is the exact silent-failure mode G1
  describes, now with an alertable symptom.
- **Prometheus labels are not metric labels.** `product`/`service` above are
  target labels; they must not be duplicated as series labels.
- **Remote-write alternative.** If crier is not reachable from `fleet-monitor`,
  the same series can be shipped by a sidecar using `prometheus/client_golang`'s
  remote-write sender or Alloy's `prometheus.scrape` → `remote_write` on the
  crier host. Choose one; do not run both.
- **`inbox_queue_*` cost.** One aggregate `SELECT` per scrape
  (`internal/registry/sqlite_store.go:547`). At >100k inbox rows this is a real
  read; either cache the read for 10s in the gauge callback or gate it behind a
  `CR_METRICS_QUEUE_GAUGES=false` opt-out. Verify with `EXPLAIN QUERY PLAN`
  that `inbox_entries(acked, expires_at)` is indexed before enabling on a
  large store.

---

## 5. Board rows filed

Rows are in `.coding-hermes/board/tasks.jsonl`, lane `monitoring`:

- **R1** — minimal viable emission: turn `/metrics` on by default (or in the
  shipped unit) and add `crier_up`, `crier_build_info`,
  `crier_store_healthy`, `crier_start_time_seconds`.
- **R2** — histograms: request/deliver/webhook/guard durations (§3.3).
- **R3** — guard health: circuit-open gauge, inflight, saturation counters.
- **R4** — webhook queue depth + federation hold age + store read errors +
  detect alert counters.
- **R5** — rename existing series to the `crier_*` convention with a dated
  dual-emit window, and fix `http_requests_total{code}` → `{code,route}`.

---

## 6. Verification — how a reviewer proves each metric is live

"Done" = every command below returns the stated result **against a deployed
crier**, not against a local build.

1. **Endpoint registered.**
   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' http://<host>:8767/metrics
   # auth on: 200 with `-H "Authorization: Bearer $CRIER_TOKEN"`, 401 without.
   # Expected: 200 (R1 changes the default; today 404 unless CR_ENABLE_METRICS=true).
   ```
2. **Exposition parses.**
   ```bash
   curl -s <host>:8767/metrics | promtool check metrics
   # Expected: exit 0, no output. (Catches malformed HELP/TYPE and duplicate series.)
   ```
3. **R1 metrics present and correct.**
   ```promql
   crier_up{product="crier"} == 1
   crier_build_info{job="crier"}                                   # exactly one series, version label = git describe
   crier_store_healthy{backend="postgres"} == 1
   time() - crier_start_time_seconds > 60
   ```
4. **Counters move.** Deliver one message and re-scrape:
   ```bash
   curl -s -X POST http://<host>:8767/agents/probe/inbox -H 'Content-Type: application/json' \
        -d '{"sender":"verify","payload":{"k":1}}'
   curl -s <host>:8767/metrics | grep -E '^crier_delivery_accepted_total|^crier_http_requests_total\{route="/agents/\{id\}/inbox"'
   # Expected: both advance by 1.
   ```
   (Verified working on the pre-rename names today: `deliveries_total 3` after
   three 201 deliveries, `inbox_queue_depth 3`, `inbox_queue_oldest_age_seconds
   0.022` — probe run 2026-11-03.)
5. **Latency is a distribution.**
   ```promql
   histogram_quantile(0.99, sum by (le, route) (rate(crier_http_request_duration_seconds_bucket[5m]))) > 1
   # Expected: empty at idle; non-empty under load — proves buckets are live, not a stub.
   ```
6. **Guard circuit trips.**
   ```promql
   crier_guard_provider_circuit_open == 1
   # Reproduce by pointing CR_GUARD_BASE_URL at a blackhole; expected within circuit_threshold (default 10) failures.
   ```
7. **Queue lag is visible.**
   ```promql
   crier_inbox_queue_oldest_age_seconds > 300
   crier_webhook_queue_oldest_age_seconds > 60
   ```
8. **Scrape is healthy from Prometheus's side.**
   ```promql
   up{job="crier"} == 1
   scrape_duration_seconds{job="crier"} < 1
   ```

---

## 7. What this audit could NOT verify

- **The deployed crier's actual flags.** No access to the production host's
  `/etc/crier/crier.env`; whether production sets `CR_ENABLE_METRICS=true` is
  **unknown**. The shipped unit does not (verified: `deploy/crier.service`), and
  `grep -rln CR_ENABLE_METRICS` over the repo finds only scripts/tests/docs/bin —
  which strongly suggests it is off, but is not proof about the running host.
- **Whether `fleet-monitor` already has a crier scrape job.** The
  `/home/kara/fleet-monitoring` tree is out of scope for this audit and was not
  read.
- **Postgres/SQLite store behaviour at scale.** The depth-query cost claim is
  from reading the SQL (`internal/registry/sqlite_store.go:547`), not a
  measurement against a large store.
- **The `countRequests` `Hijack` path under load.** It forwards `Hijack` for
  WebSocket upgrades (`cmd/server/observability.go:187-192`) and this was not
  load-tested; a WS upgrade increments `http_requests_total` with whatever
  status the recorder holds (`200`), which may mis-attribute long-lived sockets.
