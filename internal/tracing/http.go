package tracing

import (
	"bufio"
	"context"
	"net"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer(instrumentationName)

// Start creates an application span using the shared crier instrumentation scope.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !enabled.Load() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// Inject adds W3C trace context headers to an outbound request.
func Inject(ctx context.Context, header http.Header) {
	if enabled.Load() {
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
	}
}

// HTTPMiddleware traces one matched HTTP route. routeName must return the
// router's route template (or "unrouted" when no route matched).
func HTTPMiddleware(routeName func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled.Load() {
				next.ServeHTTP(w, r)
				return
			}
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			route := routeName(r)
			if route == "" {
				route = "unrouted"
			}
			name := r.Method + " " + route
			ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", route),
			))
			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				span.SetAttributes(attribute.Int("http.response.status_code", recorder.status))
				if recorder.status >= http.StatusInternalServerError {
					span.SetStatus(codes.Error, http.StatusText(recorder.status))
				}
				span.End()
			}()
			next.ServeHTTP(recorder, r.WithContext(ctx))
		})
	}
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *responseRecorder) WriteHeader(status int) {
	if !w.wrote {
		w.status = status
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
