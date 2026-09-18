package httperr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSONErrorDeclaresJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteJSONError(rec, http.StatusBadRequest, "invalid json")

	// The status assertion also pins the ORDER: writing the body before
	// WriteHeader would leave the recorder at 200.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// The stdlib helper set nosniff; the migration must not drop it.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	// Byte-identical to what net/http's Error helper produced for the same
	// body: the document plus its trailing newline.
	if got, want := rec.Body.String(), "{\"error\":\"invalid json\"}\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestWriteJSONErrorEscapesTheMessage(t *testing.T) {
	rec := httptest.NewRecorder()

	// A message that needs escaping must still produce a decodable document
	// with the original value, never a broken one.
	const msg = `bad "topic" and a \ backslash`
	WriteJSONError(rec, http.StatusBadRequest, msg)

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q is not valid JSON: %v", rec.Body.String(), err)
	}
	if payload["error"] != msg {
		t.Errorf("error = %q, want %q", payload["error"], msg)
	}
}

func TestWriteJSONErrorIsValidJSONForEveryStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		rec := httptest.NewRecorder()

		WriteJSONError(rec, status, http.StatusText(status))

		if rec.Code != status {
			t.Errorf("status = %d, want %d", rec.Code, status)
		}
		if !json.Valid(rec.Body.Bytes()) {
			t.Errorf("status %d: body %q is not valid JSON", status, rec.Body.String())
		}
	}
}
