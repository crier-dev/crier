// chatclient_test.go — CR-CHAT-009: the web chat client is served at /chat as
// self-contained HTML (the same serving pattern /docs uses).
package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestChatClientRouteServesHTML(t *testing.T) {
	baseURL := startTestServer(t)

	resp, err := http.Get(baseURL + "/chat")
	if err != nil {
		t.Fatalf("GET /chat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /chat: status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /chat: Content-Type %q, want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /chat body: %v", err)
	}
	for _, marker := range []string{
		"data-openapi-client=\"CR-CHAT-009\"", // the page's own dogfood marker
		"POST /sessions/{id}/messages",        // names the public endpoint it calls
	} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("GET /chat body missing marker %q", marker)
		}
	}
}
