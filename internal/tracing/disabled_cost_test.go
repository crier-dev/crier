package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// This file is R-T6's measurement (specs/OBSERVABILITY-TRACING.md §7 test 6).
// The claim "disabled tracing costs nothing" was asserted by construction
// (Setup early-returns before building an exporter); nothing measured the
// per-request cost of the disabled path. The benchmarks below measure it on a
// request-shaped workload, and the Test functions below gate it so that plain
// `go test` — not just an on-demand benchmark run — carries the guarantee.

// benchSink defeats dead-code elimination in the baseline benchmarks without
// adding an allocation of its own.
var benchSink int

// benchBody is the handler's response body, allocated once: the request-shaped
// benchmarks measure tracing, not the recorder or the payload.
var benchBody = []byte(`{"messages":[]}`)

// benchResponseWriter is the minimum http.ResponseWriter the request-shaped
// benchmarks drive. It is allocated once per benchmark, outside the measured
// loop.
type benchResponseWriter struct {
	header http.Header
	status int
	n      int
}

func newBenchResponseWriter() *benchResponseWriter {
	return &benchResponseWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *benchResponseWriter) reset() {
	w.status = http.StatusOK
	w.n = 0
}

func (w *benchResponseWriter) Header() http.Header { return w.header }

func (w *benchResponseWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

func (w *benchResponseWriter) WriteHeader(status int) { w.status = status }

// benchRequest is the request shape the registry retrieve path serves. The
// agent id is taken from the request itself because the production site passes
// mux.Vars(r)["id"] — also a runtime string, not a constant.
func benchRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/agents/agent-fixture-0001/inbox", nil)
}

func benchBodyStatus(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(benchBody)
}

// requireDisabled fails the caller if tracing is not in its default-off state,
// so a global provider left behind by another test cannot make these numbers
// describe the enabled path.
func requireDisabled(tb testing.TB) {
	tb.Helper()
	if _, err := Setup(context.Background(), Config{}); err != nil {
		tb.Fatalf("disable tracing: %v", err)
	}
	if enabled.Load() {
		tb.Fatal("tracing reports enabled after a default-off Setup")
	}
}

// BenchmarkDisabledRequestShape compares the same handler served four ways, all
// with tracing disabled:
//
//	Baseline/NoTracing                  the handler alone — the floor
//	Middleware                          + the server's HTTPMiddleware (main.go:359)
//	StoreHelper                         + the store-call helper with the attribute
//	                                    set the retrieve site really emits
//	StoreHelperContextPropagation        + the site's r = r.WithContext(ctx) line
//
// The last case is the request exactly as internal/registry/handler.go:1825-1827
// serves it. It is measured, not gated, because the context copy lives in the
// handler (INT-018's file) and happens whether or not tracing is on.
func BenchmarkDisabledRequestShape(b *testing.B) {
	requireDisabled(b)
	request := benchRequest()
	writer := newBenchResponseWriter()
	route := func(*http.Request) string { return "/agents/{id}/inbox" }

	run := func(b *testing.B, handler http.Handler) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			writer.reset()
			handler.ServeHTTP(writer, request)
		}
		b.StopTimer()
		benchSink += writer.status
	}

	b.Run("Baseline/NoTracing", func(b *testing.B) {
		run(b, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			benchBodyStatus(w)
		}))
	})

	b.Run("Middleware", func(b *testing.B) {
		run(b, HTTPMiddleware(route)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			benchBodyStatus(w)
		})))
	})

	b.Run("StoreHelper", func(b *testing.B) {
		run(b, HTTPMiddleware(route)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, span := Start(r.Context(), "registry.retrieve",
				attribute.String("crier.agent_id", r.URL.Path))
			span.SetAttributes(attribute.Int("crier.message_count", 3))
			defer span.End()
			benchBodyStatus(w)
		})))
	})

	b.Run("StoreHelperContextPropagation", func(b *testing.B) {
		run(b, HTTPMiddleware(route)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := Start(r.Context(), "registry.retrieve",
				attribute.String("crier.agent_id", r.URL.Path))
			span.SetAttributes(attribute.Int("crier.message_count", 3))
			defer span.End()
			// Keep the derived request alive the way the handler does: it reads
			// from r after the copy, so the copy cannot be optimized away here
			// (which is what makes this case's number honest).
			r = r.WithContext(ctx)
			benchSink += len(r.URL.Path)
			benchBodyStatus(w)
		})))
	})
}

// BenchmarkDisabledStoreHelper isolates the helper call every instrumented site
// makes — Start with the site's real attribute set, a post-hoc SetAttributes for
// a value learned later, and End — against a loop that does nothing.
func BenchmarkDisabledStoreHelper(b *testing.B) {
	requireDisabled(b)
	ctx := context.Background()
	agentID := benchRequest().URL.Path

	b.Run("Baseline/NoTracing", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchSink += len(agentID)
		}
	})

	b.Run("StartSetAttributesEnd", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, span := Start(ctx, "registry.retrieve",
				attribute.String("crier.agent_id", agentID))
			span.SetAttributes(attribute.Int("crier.message_count", 3))
			span.End()
		}
	})

	// Control for the 64 B that StartSetAttributesEnd reports: the same calls
	// with the attribute slice hoisted out of the loop. Zero allocations here
	// means the residual is the escape of the call-site's temporary variadic
	// array, not the span interface itself — no call site can hoist a dynamic
	// value such as a message count, so the production number stays 1.
	b.Run("StartSetAttributesEndHoistedSlice", func(b *testing.B) {
		hoisted := []attribute.KeyValue{attribute.Int("crier.message_count", 3)}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, span := Start(ctx, "registry.retrieve",
				attribute.String("crier.agent_id", agentID))
			span.SetAttributes(hoisted...)
			span.End()
		}
	})
}

// TestDisabledPathAllocsPerRun is the R-T6 gate. A benchmark prints a number
// once; this fails the package's plain test run if the disabled path starts
// allocating at all.
//
// The measured budget (see specs/OBSERVABILITY-TRACING.md §7 test 6):
//
//   - Start, Inject and HTTPMiddleware: 0 allocations per operation. Start used
//     to allocate 64 B per call because its variadic attrs parameter leaked to
//     the escape analysis; Start now copies the slice in the enabled branch.
//   - A post-hoc span.SetAttributes(...) adds exactly one 64 B allocation, which
//     is the variadic argument array escaping through the otel trace.Span
//     interface. That is an API-signature cost of the span interface, not
//     package code, so the instrumented-request case below allows it — and no
//     more, so a regressed Start or middleware still fails this test.
func TestDisabledPathAllocsPerRun(t *testing.T) {
	requireDisabled(t)

	ctx := context.Background()
	agentID := benchRequest().URL.Path

	t.Run("Start", func(t *testing.T) {
		allocs := testing.AllocsPerRun(1000, func() {
			_, span := Start(ctx, "registry.retrieve",
				attribute.String("crier.agent_id", agentID))
			span.End()
		})
		if allocs != 0 {
			t.Fatalf("disabled Start allocated %v allocs/op, want 0", allocs)
		}
	})

	t.Run("Inject", func(t *testing.T) {
		headers := make(http.Header)
		allocs := testing.AllocsPerRun(1000, func() {
			Inject(ctx, headers)
		})
		if allocs != 0 || headers.Get("traceparent") != "" {
			t.Fatalf("disabled Inject allocated %v allocs/op (want 0) or injected traceparent %q",
				allocs, headers.Get("traceparent"))
		}
	})

	t.Run("HTTPMiddleware", func(t *testing.T) {
		request := benchRequest()
		writer := newBenchResponseWriter()
		routeCalls := 0
		handler := HTTPMiddleware(func(*http.Request) string {
			routeCalls++
			return "/agents/{id}/inbox"
		})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			benchBodyStatus(w)
		}))
		allocs := testing.AllocsPerRun(1000, func() {
			writer.reset()
			handler.ServeHTTP(writer, request)
		})
		if allocs != 0 {
			t.Fatalf("disabled middleware allocated %v allocs/op, want 0", allocs)
		}
		if routeCalls != 0 {
			t.Fatalf("disabled middleware called routeName %d times, want 0", routeCalls)
		}
		if writer.status != http.StatusOK || writer.n != len(benchBody) {
			t.Fatalf("disabled middleware changed the response: status=%d written=%d", writer.status, writer.n)
		}
	})

	// Ceiling, not equality: the expected 1 is the documented otel-interface
	// cost of span.SetAttributes. A corrected Start or middleware still passes;
	// any new per-request allocation in package code takes it to 2 and fails.
	t.Run("InstrumentedRequestCeiling", func(t *testing.T) {
		request := benchRequest()
		writer := newBenchResponseWriter()
		handler := HTTPMiddleware(func(*http.Request) string { return "/agents/{id}/inbox" })(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, span := Start(r.Context(), "registry.retrieve",
					attribute.String("crier.agent_id", r.URL.Path))
				span.SetAttributes(attribute.Int("crier.message_count", 3))
				defer span.End()
				benchBodyStatus(w)
			}))
		const allowed = 1
		allocs := testing.AllocsPerRun(1000, func() {
			writer.reset()
			handler.ServeHTTP(writer, request)
		})
		if allocs > allowed {
			t.Fatalf("disabled instrumented request allocated %v allocs/op, want <= %d "+
				"(1 = the documented span.SetAttributes variadic escape)", allocs, allowed)
		}
	})
}
