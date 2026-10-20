// chatclient.go — the CR-CHAT-009 web client MVP: the chat surface as a
// CLIENT of the public API.
//
// The page is served at GET /chat exactly the way /docs is served: an
// embedded (go:embed), self-contained HTML document. Its trust basis map is
// rendered once from the Go-side table (no filesystem or external asset). The
// client JavaScript calls ONLY endpoints declared in docs/openapi.yaml — the
// page is a dogfood proof of the public wire, not a bespoke backend.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed chat.html
var chatClientSource []byte

// Render once, using JSON encoding for script-safe strings. Keep the marker
// unique and fail loudly on template drift instead of serving an empty map.
var chatClientHTML = func() []byte {
	marker := []byte("const basisWords = /* trust-basis-words */ {};")
	if bytes.Count(chatClientSource, marker) != 1 {
		panic("chat: expected exactly one trust basis map marker")
	}
	words, err := json.Marshal(chatTrustBasisWords)
	if err != nil {
		panic(err)
	}
	return bytes.Replace(chatClientSource, marker, append(append([]byte("const basisWords = "), words...), ';'), 1)
}()

// handleChatClient serves the embedded chat client page.
func handleChatClient(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(chatClientHTML); err != nil {
		http.Error(w, "cannot serve chat client", http.StatusInternalServerError)
	}
}

// chatBrowserRefusalTemplate is the human-readable 401 page handed to the
// auth middleware at boot (DF-CRIER-444). The page is AUTHORED inside the
// embedded chat.html, inside an inert <template id="df444-browser-refusal">
// the chat client never instantiates — one document carries both pages, no
// second asset. Extracted exactly once here, byte-exact between the markers,
// with the same fail-loud discipline as the trust-basis map above: a missing
// or duplicated template panics at init instead of serving an empty refusal.
var chatBrowserRefusalTemplate = func() string {
	open := []byte(`<template id="df444-browser-refusal">`)
	close := []byte(`</template>`)
	openAt := bytes.Index(chatClientSource, open)
	if openAt < 0 {
		panic("chat: browser refusal template missing from chat.html")
	}
	if bytes.Count(chatClientSource, open) != 1 {
		panic("chat: expected exactly one browser refusal template in chat.html")
	}
	body := chatClientSource[openAt+len(open):]
	closeAt := bytes.Index(body, close)
	if closeAt < 0 {
		panic("chat: browser refusal template is never closed in chat.html")
	}
	page := body[:closeAt]
	// The authored page starts right after the template tag with a newline and
	// is indented one level inside chat.html; trim the LEADING newline only so
	// the served document starts at <!DOCTYPE html>. Interior indentation is
	// part of the page and is served as authored.
	page = bytes.TrimPrefix(page, []byte("\n"))
	// Same for the trailing newline the authored layout puts before
	// </template>: the page ends at </html>, not after it.
	page = bytes.TrimRight(page, "\n")
	if !bytes.HasPrefix(page, []byte("<!DOCTYPE html>")) {
		panic("chat: browser refusal template does not start with a DOCTYPE")
	}
	if !bytes.HasSuffix(page, []byte("</html>")) {
		panic("chat: browser refusal template does not end with </html>")
	}
	return string(page)
}()
