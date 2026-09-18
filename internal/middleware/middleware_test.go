package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLogs redirects slog default output into a buffer for the duration of
// the test. Uses a TextHandler so log lines stay readable and assertion
// substrings can match on key=value pairs.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})

	return &buf
}

func TestLoggingLogsSuccessfulGET(t *testing.T) {
	logs := captureLogs(t)
	handler := Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/success", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(logs.String(), "method=GET path=/success status=200") {
		t.Fatalf("log = %q, want method, path, and status", logs.String())
	}
}

func TestLoggingCapturesNonOKStatus(t *testing.T) {
	logs := captureLogs(t)
	handler := Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if !strings.Contains(logs.String(), "method=GET path=/missing status=404") {
		t.Fatalf("log = %q, want non-OK status", logs.String())
	}
}

func TestLoggingLogsRequestDuration(t *testing.T) {
	logs := captureLogs(t)
	handler := Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/timed", nil)

	handler.ServeHTTP(recorder, request)

	// Verify duration field is present and parses as a positive Go duration <1s.
	durStr, ok := extractAttr(logs.String(), "duration")
	if !ok {
		t.Fatalf("log = %q, want duration field", logs.String())
	}
	duration, err := time.ParseDuration(durStr)
	if err != nil {
		t.Fatalf("parse logged duration %q: %v", durStr, err)
	}
	if duration <= 0 {
		t.Errorf("logged duration = %s, want > 0", duration)
	}
	if duration >= time.Second {
		t.Errorf("logged duration = %s, want < 1s", duration)
	}
}

func TestLoggingPassesRequestBodyToHandler(t *testing.T) {
	captureLogs(t)
	var (
		gotBody []byte
		readErr error
	)
	handler := Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader("request payload"))

	handler.ServeHTTP(recorder, request)

	if readErr != nil {
		t.Fatalf("read request body: %v", readErr)
	}
	if string(gotBody) != "request payload" {
		t.Fatalf("handler body = %q, want %q", gotBody, "request payload")
	}
}

func TestRecoveryCatchesPanicAndReturnsJSONError(t *testing.T) {
	logs := captureLogs(t)
	handler := Recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body["error"] != "internal server error" {
		t.Fatalf("error = %q, want %q", body["error"], "internal server error")
	}
	// DF-CRIER-212: the body is JSON, so the response has to say so. net/http's
	// Error helper hard-codes "text/plain; charset=utf-8" and OVERWRITES any
	// Content-Type set before it, so a JSON body served that way reaches the
	// client mislabelled — the half the body assertion above could not see.
	if ct := recorder.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q — the recovered-panic body is JSON", ct, "application/json")
	}
	// The wire body is byte-identical to what the pre-fix build (b140a6f)
	// wrote: the literal below was captured from that build, so this pins the
	// migration off the stdlib helper as headers-only.
	if got, want := recorder.Body.String(), "{\"error\":\"internal server error\"}\n"; got != want {
		t.Errorf("body = %q, want %q (pre-fix wire bytes)", got, want)
	}
	if !strings.Contains(logs.String(), `error=boom`) {
		t.Fatalf("log = %q, want recovered panic", logs.String())
	}
}

func TestRecoveryPassesThroughNormalRequest(t *testing.T) {
	handler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test", "passed")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/normal", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	if recorder.Header().Get("X-Test") != "passed" {
		t.Fatalf("X-Test header = %q, want %q", recorder.Header().Get("X-Test"), "passed")
	}
	if recorder.Body.String() != "created" {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), "created")
	}
}

func TestRecoveryHandlesRequestAfterPanic(t *testing.T) {
	captureLogs(t)
	calls := 0
	handler := Recovery(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			panic("first request")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("second request succeeded"))
	}))

	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, httptest.NewRequest(http.MethodGet, "/first", nil))
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, httptest.NewRequest(http.MethodGet, "/second", nil))

	if firstRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusInternalServerError)
	}
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
	if secondRecorder.Body.String() != "second request succeeded" {
		t.Fatalf("second body = %q, want successful response", secondRecorder.Body.String())
	}
	if calls != 2 {
		t.Fatalf("handler calls = %d, want 2", calls)
	}
}

func TestResponseWriterWriteHeaderCapturesStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	wrapped := &responseWriter{ResponseWriter: recorder, status: http.StatusOK}

	wrapped.WriteHeader(http.StatusTeapot)

	if wrapped.status != http.StatusTeapot {
		t.Fatalf("captured status = %d, want %d", wrapped.status, http.StatusTeapot)
	}
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("response status = %d, want %d", recorder.Code, http.StatusTeapot)
	}
}

func TestResponseWriterDefaultsToOKWhenWritingBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	wrapped := &responseWriter{ResponseWriter: recorder, status: http.StatusOK}

	if _, err := wrapped.Write([]byte("body")); err != nil {
		t.Fatalf("write body: %v", err)
	}

	if wrapped.status != http.StatusOK {
		t.Fatalf("captured status = %d, want default %d", wrapped.status, http.StatusOK)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("response status = %d, want default %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != "body" {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), "body")
	}
}

// TestLogging_PanickedRequestIsLogged is the regression test supplied by the
// reporter (GitHub issue #1, item 2): Recovery sits OUTSIDE Logging in the
// production chain (cmd/server registers RequestID → Auth → Recovery →
// Logging), so a handler panic unwinds through Logging before a sequential log
// write could run — the client still gets its 500 from Recovery, but the
// request was missing from the access log entirely. The write is now deferred,
// so the panicked request appears with status 500, its path and a correlation
// id that joins it to the "panic recovered" line.
func TestLogging_PanickedRequestIsLogged(t *testing.T) {
	logs := captureLogs(t)
	handler := RequestID(Recovery(Logging(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(logs.String(), "msg=request") {
		t.Fatalf("log = %q, want an access-log line for the panicked request", logs.String())
	}

	line := logLineContaining(t, logs.String(), "msg=request")
	if !strings.Contains(line, "status=500") {
		t.Errorf("access-log line = %q, want status=500", line)
	}
	if !strings.Contains(line, "path=/agents") {
		t.Errorf("access-log line = %q, want path=/agents", line)
	}
	id, ok := extractAttr(line, "request_id")
	if !ok || id == "" {
		t.Errorf("access-log line = %q, want a non-empty request_id", line)
	}

	// The panic was not swallowed: Recovery caught it and its line carries the
	// same correlation id, so the 500 and the access log can be joined.
	panicLine := logLineContaining(t, logs.String(), "panic recovered")
	panicID, ok := extractAttr(panicLine, "request_id")
	if !ok || panicID != id {
		t.Errorf("panic line request_id = %q, want %q (same id as the access log)", panicID, id)
	}
}

// logLineContaining returns the first captured log line containing needle.
func logLineContaining(t *testing.T, logs, needle string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no log line containing %q in %q", needle, logs)
	return ""
}

// extractAttr pulls the value of a key from a slog text-encoded line.
// Matches the textual form `<key>=<value>` followed by a space, end of line,
// or a quote (since slog text format escapes values containing `=` or spaces).
func extractAttr(line, key string) (string, bool) {
	prefix := key + "="
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return "", false
	}
	rest := line[idx+len(prefix):]
	if rest == "" {
		return "", false
	}
	// Quoted value (slog text format escapes values containing `=`).
	if rest[0] == '"' {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return "", false
		}
		return rest[1 : 1+end], true
	}
	// Bare value up to the next space, newline, or end of line.
	end := len(rest)
	for i, r := range rest {
		if r == ' ' || r == '\n' || r == '\r' {
			end = i
			break
		}
	}
	return rest[:end], true
}
