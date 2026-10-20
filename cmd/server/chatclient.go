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
