package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Exercise the served artifact, not a second copy of the page. Behavioral
// state/escaping coverage is in scripts/chat-management-browser.cjs.
func TestChatManagementRoutesInServedScript(t *testing.T) {
	parallelBoot(t)
	resp, err := http.Get(startTestServer(t) + "/chat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /chat: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.SplitN(string(body), "<script>", 2)
	if len(script) != 2 {
		t.Fatal("served page has no script")
	}
	for _, marker := range []string{
		`"/principals"`, `"/bindings"`, `"/grants"`, `"/fed/peers"`,
		`"/trust/anchors"`, `"/trust/verify"`, `"/agents/" + encodeURIComponent`,
		`"/class"`, `refreshManagement();`, `"refused"`, `"empty"`, `"populated"`,
		`"error"`, `CR_PERMISSIONS_ADMIN_TOKEN`, `error.detail = body.detail`,
		// DF-CRIER-384: the Authority view is a fold, and its table cells pass
		// through the token-material guard. A served page that lost either is the
		// defect this row was filed for, so both are pinned here.
		`authority-view`, `authorityOpen()`, `managementCell`, `managementFieldLabel`,
		`(short prefix)`, `SHORT_FINGERPRINT`, `TOKEN_WITHHELD`,
	} {
		if !strings.Contains(script[1], marker) {
			t.Errorf("served management script missing %q", marker)
		}
	}
	if strings.Contains(script[1], `esc(managementValue(row[field]))`) {
		t.Errorf("the management table renders raw row values again: the token-material guard (managementCell) is bypassed")
	}
}
