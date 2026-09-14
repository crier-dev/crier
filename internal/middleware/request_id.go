package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HeaderRequestID is the request-correlation header (DF-CRIER-141): an
// inbound X-Request-Id is adopted as this request's correlation id so a
// caller's own trace id survives a hop, and every response carries the id —
// the server's own when the client supplied none.
const HeaderRequestID = "X-Request-Id"

// maxRequestIDLen bounds an accepted inbound correlation id. An oversized
// value is replaced rather than echoed, so a client cannot inject an
// unbounded string into every log line of the request.
const maxRequestIDLen = 128

// requestIDKey is the unexported context key for the correlation id, so no
// other package can collide with it by using a bare string key.
type requestIDKey struct{}

// WithRequestID returns ctx carrying id as the request correlation id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the correlation id carried by ctx, or "" when
// the request never passed through the RequestID middleware (background
// work, tests, internal calls).
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID returns middleware that gives every request a correlation id:
// the inbound X-Request-Id when the client sent one, otherwise a freshly
// generated id. The id lands in the request context (RequestIDFromContext)
// and is echoed on the response as X-Request-Id.
//
// Wire it OUTERMOST (registered before Auth/Recovery/Logging in cmd/server)
// so every response — including 401s and the public /health and /version
// endpoints — carries an id, and so the request log line can be joined to
// the handler's own log lines for the same request.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if len(id) > maxRequestIDLen {
			// Oversized input: generate our own instead of echoing it into
			// every log line this request produces.
			id = ""
		}
		if id == "" {
			id = GenerateRequestID()
		}
		// Set before calling next: the echo must be in place before any
		// handler (or Auth/Recovery) writes the status line.
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// GenerateRequestID returns a 24-character lowercase hex correlation id (12
// bytes from crypto/rand — the same shape and source the registry and mesh
// use for message ids). A crypto/rand failure, which does not occur on a
// healthy host, falls back to a time-derived id rather than panicking or
// returning an empty id: a request is never left uncorrelated.
func GenerateRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
