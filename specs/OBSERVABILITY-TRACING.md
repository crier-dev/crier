# OBSERVABILITY-TRACING — OpenTelemetry trace export (opt-in, zero default overhead)

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 (agreed: trace export beside metrics) · INT-009

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
`Setup` installs the SDK, so disabled-mode spans are `noop.Span` (zero allocation, zero network).

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
6. Benchmark: `go test -bench` on the deliver path with tracing disabled is within noise of
   pre-change HEAD (assert with `-benchmem` before/after comparison recorded in the PR evidence).

## 8. Coverage table

| R# | Statement | Assertion |
|---|---|---|
| R-T1 | Default off = no SDK, noop provider | test: 1 |
| R-T2 | Enabled = exported span tree with semconv attrs | test: 2 |
| R-T3 | Inbound traceparent joins the trace | test: 3 |
| R-T4 | No payload attributes, ever | test: 4 (check: §4 allowlist) |
| R-T5 | Bad tracing config refuses startup | test: 5 |
| R-T6 | Disabled overhead within noise | test: 6 |
| R-T7 | Tracing failures never affect delivery | check: §5 row 2 + error-catalog audit |

Out of scope (binding): replacing or wrapping `internal/metrics` with the OTel meter; log
correlation (trace_id in slog) — follow-up if wanted; metrics export over OTLP; A2A-relay-span
propagation beyond crier's own surface.
