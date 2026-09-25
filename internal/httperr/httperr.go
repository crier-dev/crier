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
	WriteJSON(w, status, map[string]string{"error": msg})
}

// WriteJSON writes body as the response entity with the given status, under
// the SAME header contract WriteJSONError established: an explicit
// application/json Content-Type (net/http's Error helper cannot serve one) and
// X-Content-Type-Options: nosniff.
//
// It exists for the error bodies that carry more than the message — the router
// fallbacks' shape hints (CR-FEAT-031) — so the header contract stays in one
// place instead of being re-derived at the call site. A struct body keeps the
// caller's field order on the wire, which a map[string]string would sort.
//
// An unencodable body is unreachable for the string-field structs written
// through this package. It is not a panic: the caller of this function is an
// error path (the router's 404/405 fallbacks are deliberately outside
// middleware.Recovery), so a panic here would turn a 404 into a connection
// reset. The bare envelope keeps the response decodable and the status the
// caller chose.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		buf = []byte(`{"error":"error response could not be encoded"}`)
	}
	// The newline terminates the body, matching both the stdlib helper's bytes
	// and json.Encoder.Encode.
	buf = append(buf, '\n')

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
