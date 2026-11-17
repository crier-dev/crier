# OBSERVABILITY-TRACING — OpenTelemetry trace export (opt-in, zero default overhead)

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 (agreed: trace export beside metrics) · INT-009
· test 6 measured 2026-11-17 (INT-024)

## 1. Purpose

Crier serves `/metrics` (hand-rolled, zero-dependency Prometheus text, `internal/metrics/`) but
has zero distributed tracing — verified at HEAD `7e4c3f4`: `grep -rn 'otel|opentelemetry' --include='*.go' .`
returns no hits. Multi-hop deliveries (ingress → guard → store → webhook/harness/federation)
are debugged from logs alone. This spec adds OpenTelemetry trace export beside metrics: opt-in
via env, byte-identical behaviour and no SDK goroutines when off.

## 2. Amendments to existing docs

- `docs/observability-spec.md` (Status: proposed, audit 2026-11-03, HEAD `104a2096`) defines
  metrics only and has no trace mentions. This spec EXTENDS it; its metric series and its §3.4
  "deliberately NOT instrumented" list (no payload labels, no per-message labels) are adopted
  verbatim for span attributes. Its identity series (`crier_up`, `crier_build_info`) are unaffected.
- `internal/metrics` stays dependency-free. The OTel SDK lives behind the tracing gate only;
  no metric series moves to the OTel meter.

## 3. Interface

```go
// internal/tracing/tracing.go
// Setup initialises the global trace provider. Called once from run() before
// the server starts. When cfg.Enabled is false it installs the no-op provider
// (the go.opentelemetry.io/otel default) and returns without starting the SDK.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error)

type Config struct {
	Enabled     bool    // CR_TRACING_ENABLED, tolerant bool, default false
	Exporter    string  // CR_TRACING_EXPORTER: "otlp" | "stdout", default "otlp"
	OTLPEndpoint string // CR_TRACING_OTLP_ENDPOINT, default "http://localhost:4318"
	SampleRatio float64 // CR_TRACING_SAMPLE_RATIO, default 1.0, (0,1]
	ServiceName string  // CR_TRACING_SERVICE_NAME, default "crier"
}
```

Package tracer used by all instrumentation sites:
`var tracer = otel.Tracer("github.com/crier-dev/crier")` — resolves to the no-op provider until
`Setup` installs the SDK, so disabled-mode spans are `noop.Span`. Disabled-mode `Start` allocates
nothing (measured, §7 test 6); the only allocation left on the disabled path is the 64 B temporary
of a call site's own post-hoc `span.SetAttributes(...)` argument, so "zero allocation" holds for
this package and one 64 B per post-hoc attribute holds for instrumented sites.

## 4. Wiring

- Dependencies added to go.mod: `go.opentelemetry.io/otel`, `.../otel/sdk`,
  `.../otel/exporters/otlp/otlptrace/otlptracehttp`, `.../otel/trace`. No other new deps.
- main.go: `shutdown, err := tracing.Setup(ctx, cfg.Tracing)` immediately after `config.LoadFrom`
  (`cmd/server/main.go:247`); `defer shutdown(ctx)`; on config error exit 1 (fail-closed: enabled
  with an unparseable sample ratio refuses startup).
- Export path: `otlptracehttp` client → `sdktrace.NewBatchSpanProcessor` (default batch: 5s
  delay, 512 queue — SDK defaults, not re-tuned). `stdout` exporter exists for tests and local
  debugging only.
- Sampler: `sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))`.

### Instrumentation points (first wave, exact sites)

| Span name | Where | Attributes (semconv) |
|---|---|---|
| `POST /ingress/messages` (and every route) | new HTTP middleware in `cmd/server` wrapping the mux, named by route template | `http.request.method`, `http.route`, `http.response.status_code` |
| `registry.deliver` | `registry.Store` Deliver wrapper where the registry handler calls the store | `crier.agent_id` |
| `registry.retrieve` / `registry.ack` | same store wrapper | `crier.agent_id`, `crier.message_count` |
| `webhook.deliver` | webhook driver's outbound request | `crier.webhook.outcome` |
| `harness.dispatch` | `harness.Registry.Deliver` (`internal/harness/registry.go:222`) | `crier.harness.adapter`, `crier.harness.state` |
| `relay.publish` / `relay.subscribe` | `internal/relay` handler entry | `crier.topic` |

Attribute rules: **never** message payloads, prompt text, auth headers, or URL query strings
(extends observability-spec §3.4 to spans). Errors: `span.RecordError(err)` +
`span.SetStatus(codes.Error, ...)`; spans never alter delivery behaviour or status codes.

## 5. Error Catalog

| Condition | Behaviour |
|---|---|
| Tracing disabled (default) | no SDK init, no goroutines, no spans buffered — noop provider |
| Enabled, OTLP endpoint unreachable | exports retried by the SDK's builtin retry; failures logged at debug; server runs normally — tracing is never load-bearing |
| Bad `CR_TRACING_SAMPLE_RATIO` (0, ≤0, >1, unparsable) | startup exit 1 |
| Bad `CR_TRACING_EXPORTER` value | startup exit 1 |
| `shutdown` error at exit | logged, does not mask the process exit code |

## 6. Edge Cases

- Request without a traceparent header: a root span is created (per-sample-ratio decision).
- Request WITH a valid traceparent: the middleware span joins the caller's trace (extracts the
  remote context) — crier is a good citizen of an existing trace.
- `http.route` for unrouted 404s: attribute value `unrouted`, span name `UNROUTED`.
- Very long deliveries (harness exec up to `CR_HARNESS_TIMEOUT`): the span lives for the delivery;
  the batch processor is non-blocking, so a slow delivery never blocks the batch queue beyond
  SDK defaults.
- Fan-out (session message → N inbox deliveries): one HTTP server span; each store call is a
  child span; no per-recipient spans beyond the store children.

## 7. Testing

1. Test that with `CR_TRACING_ENABLED` unset, `tracing.Setup` returns a nil-effect shutdown and
   `otel.GetTracerProvider()` is the global no-op (no SDK goroutines: `runtime.NumGoroutine`
   stable across Setup).
2. Test with the `stdout` exporter: a `POST /ingress/messages` in `httptest` produces a span tree
   containing `POST /ingress/messages` → `registry.deliver`, with `http.route` and
   `http.response.status_code` set.
3. Test that an inbound `traceparent` header is honored (extracted trace id equals the server
   span's trace id).
4. Test that no span attribute key contains a message payload — assert the recorded attribute
   key set against the §4 allowlist for every instrumented site.
5. Test startup refusal for `CR_TRACING_SAMPLE_RATIO=2` and `CR_TRACING_EXPORTER=grpc`.
6. Disabled-path cost — **measured**, not asserted by construction. The earlier draft said "within
   noise" and justified it with the fact that `Setup` early-returns before building an exporter,
   which measures nothing about a request. Evidence lives in `internal/tracing/disabled_cost_test.go`.

   Measurement with tracing at its default off (`Config{}`, `enabled == false`):

   - `Start` with the attribute set the sites really pass: **0 B, 0 allocations** per span. Before
     this measurement it was **64 B / 1 allocation**: the variadic `attrs` parameter was "leaking"
     to the escape analysis because it went straight into `trace.WithAttributes`, so every caller
     heap-allocated its attribute array even with tracing disabled. `Start` now copies the slice in
     the enabled branch, which keeps the caller's array on the caller's stack.
   - `HTTPMiddleware`, disabled: **0 B, 0 allocations** per request. It returns before calling
     `routeName` and before wrapping the ResponseWriter.
   - Post-hoc `span.SetAttributes(attribute.Int(...))` at a call site: **64 B, 1 allocation** per
     call. This is the call site's own temporary argument array escaping through the sealed
     `otel/trace.Span` interface — the same work the enabled path does to build attributes — not
     this package's code, and not removable from here (`trace.Span` embeds an unexported otel
     interface, so no local implementation or helper can keep that array on the stack). Proven to
     be the array and not the interface call: with the attribute slice hoisted out of the loop the
     identical sequence measures 0 B / 0 allocations.
   - Net for the retrieve shape (`HTTPMiddleware` + `registry.retrieve` + `message_count`):
     **64 B / 1 allocation**, and 57–106 ns/op against 5.8–9.8 ns/op for the same handler with no
     tracing. The 64 B allocation, not the tracing logic, is what widens that gap on a loaded box.
   - The retrieve site's `r = r.WithContext(ctx)` added no measurable allocation in this shape
     (same 64 B as without it). It is kept because it is what carries the span into downstream
     calls when tracing is on; the real handler passes that request to the store, where it may
     escape, so this number is a floor for that line, not a guarantee.

   Commands (machine: AMD Ryzen 7 7840HS, 16 threads; the box was **loaded** — loadavg 27–30 and
   spiking to 40+ from a build/test fleet sharing it — so ns/op is noisy, allocations are the
   deterministic part, and the figures below are observed bands, not a quiet-machine floor):

   ```
   go test ./internal/tracing -bench . -benchtime 200x -run '^$'
   go test ./internal/tracing -bench . -benchtime 20000x -count=5 -run '^$'
   ```

   `-benchtime 200x` output (2026-11-17T08:02:12Z, loadavg 29.3):

   ```
   BenchmarkDisabledRequestShape/Baseline/NoTracing-16          200   15.03 ns/op    0 B/op   0 allocs/op
   BenchmarkDisabledRequestShape/Middleware-16                   200   21.84 ns/op    0 B/op   0 allocs/op
   BenchmarkDisabledRequestShape/StoreHelper-16                  200  189.2  ns/op   64 B/op   1 allocs/op
   BenchmarkDisabledRequestShape/StoreHelperContextPropagation-16 200 163.6 ns/op   64 B/op   1 allocs/op
   BenchmarkDisabledStoreHelper/Baseline/NoTracing-16             200    2.005 ns/op   0 B/op   0 allocs/op
   BenchmarkDisabledStoreHelper/StartSetAttributesEnd-16          200  165.5  ns/op   64 B/op   1 allocs/op
   BenchmarkDisabledStoreHelper/StartSetAttributesEndHoistedSlice-16 200 26.75 ns/op  0 B/op   0 allocs/op
   ```

   Five-run band at `-benchtime 20000x` (loadavg 27.8): baseline request 5.8–9.8 ns; middleware
   9.9–15.0 ns (delta +2…+5 ns, ranges abut); `StoreHelper` 64.9–105.5 ns / 64 B / 1 alloc;
   `StoreHelperContextPropagation` 57.2–105.7 ns / 64 B / 1 alloc; helper baseline 0.24–1.54 ns;
   `StartSetAttributesEnd` 60.8–81.1 ns / 64 B / 1 alloc; hoisted-slice control 16.5–17.9 ns / 0 B.

   A benchmark is not a gate, so `TestDisabledPathAllocsPerRun` carries the guarantee in plain
   `go test`: `testing.AllocsPerRun` over `Start` (real attribute set), `Inject` and
   `HTTPMiddleware` must return **0** allocations, and the instrumented-request ceiling must stay
   at **≤ 1** (the documented `SetAttributes` array). Red-proved both directions: reverting the
   `Start` copy fails with `disabled Start allocated 1 allocs/op, want 0` and
   `instrumented request allocated 2 allocs/op, want <= 1`; adding one allocation to the disabled
   middleware branch fails with `disabled middleware allocated 1 allocs/op, want 0`.

## 8. Coverage table

| R# | Statement | Assertion |
|---|---|---|
| R-T1 | Default off = no SDK, noop provider | test: 1 |
| R-T2 | Enabled = exported span tree with semconv attrs | test: 2 |
| R-T3 | Inbound traceparent joins the trace | test: 3 |
| R-T4 | No payload attributes, ever | test: 4 (check: §4 allowlist) |
| R-T5 | Bad tracing config refuses startup | test: 5 |
| R-T6 | Disabled overhead measured: 0 allocs in Start/Inject/middleware, 1×64 B per post-hoc SetAttributes | test: 6 (bench + `TestDisabledPathAllocsPerRun`) |
| R-T7 | Tracing failures never affect delivery | check: §5 row 2 + error-catalog audit |

Out of scope (binding): replacing or wrapping `internal/metrics` with the OTel meter; log
correlation (trace_id in slog) — follow-up if wanted; metrics export over OTLP; A2A-relay-span
propagation beyond crier's own surface.
