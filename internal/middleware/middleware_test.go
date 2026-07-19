package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()

	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	return &logs
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
	if !strings.Contains(logs.String(), "GET /success 200 ") {
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
	if !strings.Contains(logs.String(), "GET /missing 404 ") {
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

	fields := strings.Fields(strings.TrimSpace(logs.String()))
	if len(fields) != 4 {
		t.Fatalf("log fields = %q, want method, path, status, and duration", fields)
	}
	duration, err := time.ParseDuration(fields[3])
	if err != nil {
		t.Fatalf("parse logged duration %q: %v", fields[3], err)
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
	if !strings.Contains(logs.String(), "panic: boom") {
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
