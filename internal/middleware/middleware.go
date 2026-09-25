package middleware

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/crier-dev/crier/internal/httperr"
)

// Logging logs each request with method, path, status, duration and the
// X-Request-Id correlation id (DF-CRIER-141). Register it inside RequestID so
// the id is always present.
//
// The write is DEFERRED (DF-CRIER-184): Logging is registered INNERMOST, so a
// handler panic unwinds through it and a plain sequential write would be
// skipped — the 500 the client receives from Recovery would never reach the
// access log, leaving only a bare "panic recovered" line with no method, path,
// status, duration or request_id. On the panicking path the line is logged
// with status 500 (nothing else was written) and the panic value is re-raised
// so Recovery still catches it: this middleware never swallows a panic.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			panicked := recover()
			status := wrapped.status
			if panicked != nil {
				// The handler never completed: the client is served the 500
				// that Recovery writes on the way out.
				status = http.StatusInternalServerError
			}
			slog.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration", time.Since(start),
				"request_id", RequestIDFromContext(r.Context()),
			)
			if panicked != nil {
				// Not ours to handle: Recovery sits outside Logging.
				panic(panicked)
			}
		}()
		next.ServeHTTP(wrapped, r)
	})
}

// Recovery catches panics and returns 500. The panic line carries the request
// correlation id so a 500 can be joined to the access-log line the same
// request produced (DF-CRIER-141/184).
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic recovered", "error", err, "request_id", RequestIDFromContext(r.Context()))
				// The 500 body is JSON, so it is written with httperr rather
				// than net/http's Error helper, which hard-codes
				// "text/plain; charset=utf-8" (DF-CRIER-212).
				httperr.WriteJSONError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

// Hijack implements http.Hijacker so WebSocket upgrades (gorilla/websocket)
// work through the middleware chain. Without it, Upgrade fails with
// "response does not implement http.Hijacker" → HTTP 500 on WS endpoints.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Flush implements http.Flusher so a streaming handler — Server-Sent Events —
// can deliver a frame before it returns. Embedding http.ResponseWriter does not
// promote Flush (it is not part of that interface), so without this the wrapper
// HIDES the underlying capability: a streaming handler's `w.(http.Flusher)`
// assertion fails and it cannot stream at all (INT-A2A-003 measured exactly
// that — 500 "this server cannot stream" on every A2A stream). Same class of
// hidden capability as Hijack above, same answer: pass the call through when the
// underlying writer supports it, and stay silent when it does not.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
