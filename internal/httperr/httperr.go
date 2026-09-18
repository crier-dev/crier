// Package httperr writes JSON error responses.
//
// It exists because net/http's Error helper cannot serve a JSON body:
// Error(w, body, code) unconditionally sets Content-Type to
// "text/plain; charset=utf-8" (and X-Content-Type-Options: nosniff) before
// WriteHeader, so it OVERWRITES any Content-Type a handler set first and every
// rejection it writes reaches a strict JSON client mislabelled as text
// (DF-CRIER-212).
package httperr

import (
	"encoding/json"
	"net/http"
)

// WriteJSONError writes {"error": msg} with the given status and an explicit
// application/json Content-Type.
//
// The wire bytes match what net/http's Error helper produced for the same body
// at every migrated call site: the compact single-key object followed by a
// newline — json.Marshal produces the object and the newline terminates it,
// which is also how the other JSON surfaces in this repo end a body
// (json.Encoder.Encode appends one). Moving a call site off the stdlib helper
// therefore changes the Content-Type header and nothing else.
//
// X-Content-Type-Options: nosniff is preserved because the stdlib helper set
// it: dropping it here would be a silent loss of a hardening header.
func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	// Marshalling a single-key map[string]string cannot fail.
	body, _ := json.Marshal(map[string]string{"error": msg})
	body = append(body, '\n')

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
