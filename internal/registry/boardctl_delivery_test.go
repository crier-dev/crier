package registry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/boardbridge"
	"github.com/gorilla/mux"
)

func boardctlDeliveryHarness(t *testing.T, enabled, allowSender bool) (http.Handler, Store, string) {
	t.Helper()
	store := setupTestStore(t)
	_, pubKey := newTestPubKey(t)
	boardDir := t.TempDir()
	allowed := []string(nil)
	if allowSender {
		allowed = []string{"sender"}
	}
	cfg := &boardbridge.Config{BoardDir: boardDir, SenderAllowlist: allowed}
	if err := store.Register(&Agent{ID: "board-agent", PublicKey: HexKey(pubKey), Boardctl: cfg}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(store)
	counter := filepath.Join(t.TempDir(), "calls")
	if enabled {
		bin := filepath.Join(t.TempDir(), "boardctl")
		script := "#!/bin/sh\nif [ \"$3\" = doctor ]; then exit 0; fi\nif [ \"$1 $2\" = 'version --json' ]; then printf '{}\\n'; exit 0; fi\nif [ \"$3\" = header ]; then printf '{}\\n'; exit 0; fi\necho called >> \"$COUNTER\"\nprintf '{\\\"ok\\\":true}\\n'\n"
		if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("COUNTER", counter)
		bridge, err := boardbridge.New(cfg, bin)
		if err != nil {
			t.Fatal(err)
		}
		handler.SetBoardctlBridge(bridge)
	}
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods(http.MethodPost)
	return router, store, counter
}

func TestBoardctlGateOffAndNonAllowlistedMessageInboxLandUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gateEnv string
		allowed bool
	}{
		{name: "gate off", gateEnv: "false", allowed: true},
		{name: "non-allowlisted sender", gateEnv: "true", allowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CR_BOARDCTL_BRIDGE_ENABLED", tc.gateEnv)
			router, store, counter := boardctlDeliveryHarness(t, tc.gateEnv == "true", tc.allowed)
			payload := json.RawMessage(`{"v":1,"op":"list","args":{}}`)
			body, _ := json.Marshal(map[string]any{"payload": payload, "sender": "sender"})
			req := httptest.NewRequest(http.MethodPost, "/agents/board-agent/inbox", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("delivery status=%d body=%s", rec.Code, rec.Body.String())
			}
			entries, _, err := store.Retrieve("board-agent", time.Minute, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("inbox entry count=%d, want 1", len(entries))
			}
			if string(entries[0].Payload) != string(payload) {
				t.Fatalf("inbox payload=%q, want unchanged payload %s", entries[0].Payload, payload)
			}
			if _, err := os.Stat(counter); !os.IsNotExist(err) {
				t.Fatalf("boardctl executed despite inert delivery: stat=%v", err)
			}
		})
	}
}
