// chatclient.go — the CR-CHAT-009 web client MVP: the chat surface as a
// CLIENT of the public API.
//
// The page is served at GET /chat exactly the way /docs is served: an
// embedded (go:embed), self-contained HTML document written out byte-for-byte on every
// request (no template, no filesystem dependency, no external asset). The
// client JavaScript calls ONLY endpoints declared in docs/openapi.yaml — the
// page is a dogfood proof of the public wire, not a bespoke backend.
package main

import (
	_ "embed"
	"net/http"
)

//go:embed chat.html
var chatClientHTML []byte

// handleChatClient serves the embedded chat client page.
func handleChatClient(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(chatClientHTML); err != nil {
		http.Error(w, "cannot serve chat client", http.StatusInternalServerError)
	}
}
