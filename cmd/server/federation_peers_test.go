package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/registry"
)

func TestFederation363ProductionRoutesAndOperatorAuth(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyfile := filepath.Join(dir, "instance.pem")
	if err = os.WriteFile(keyfile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AuthToken: "message"}
	cfg.Permissions.AdminToken = "admin"
	cfg.Federation.Name = "local"
	cfg.Federation.SelfKeyFile = keyfile
	cfg.Federation.RecordsFile = filepath.Join(dir, "peers.json")
	r := mux.NewRouter()
	var records *federation.PeerStore
	r.Use(federation.DeliveryAuth(middleware.AuthTokens(cfg.AuthToken, cfg.Permissions.AdminToken), func() *federation.PeerStore { return records }))
	hs, err := configureFederationPeers(cfg, r, registry.NewHandler(registry.NewMemoryStore()))
	if err != nil || hs == nil {
		t.Fatalf("wiring %v", err)
	}
	records = hs.Store
	for _, test := range []struct{ method, path, body string }{
		{"GET", "/fed/peers", ""}, {"POST", "/fed/peers", `{}`}, {"PATCH", "/fed/peers/a", `{}`},
		{"POST", "/fed/handshake/start", `{}`}, {"POST", "/fed/handshake/finish", `{}`},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			for _, headers := range []struct {
				bearer, operator string
				want             int
			}{{"", "", 401}, {"message", "", 403}, {"message", "wrong", 403}, {"admin", "", 403}} {
				req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
				if headers.bearer != "" {
					req.Header.Set("Authorization", "Bearer "+headers.bearer)
				}
				req.Header.Set("X-Crier-Fed-Operator", headers.operator)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != headers.want {
					t.Fatalf("gate got %d want %d: %s", w.Code, headers.want, w.Body.String())
				}
			}
		})
	}
	body := fmt.Sprintf(`{"id":"peer-a","public_key":%q,"url":"https://a.example","direction":"both","label":"Peer A"}`, hex.EncodeToString(pub))
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer message")
		req.Header.Set("X-Crier-Fed-Operator", "admin")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := call("POST", "/fed/peers", body)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	w = call("GET", "/fed/peers", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"peer-a"`) {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	w = call("PATCH", "/fed/peers/peer-a", `{"state":"revoked","label":"Revoked A"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"revoked"`) {
		t.Fatalf("patch %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/fed/handshake/start", "/fed/handshake/finish"} {
		w = call("POST", path, `{}`)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "FED_HANDSHAKE_") {
			t.Fatalf("handshake wiring %s %d %s", path, w.Code, w.Body.String())
		}
	}
	// Corruption and an unreadable instance key are boot errors, not a
	// fallback to unsigned/token auth.
	if err = os.WriteFile(cfg.Federation.RecordsFile, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = configureFederationPeers(cfg, mux.NewRouter(), registry.NewHandler(registry.NewMemoryStore())); err == nil {
		t.Fatal("corrupt store accepted")
	}
	cfg.Federation.SelfKeyFile = filepath.Join(dir, "missing")
	if _, err = configureFederationPeers(cfg, mux.NewRouter(), registry.NewHandler(registry.NewMemoryStore())); err == nil {
		t.Fatal("missing instance key accepted")
	}
}

func TestFederation363OffRegistersNoManagementSurface(t *testing.T) {
	r := mux.NewRouter()
	hs, err := configureFederationPeers(config.Config{}, r, registry.NewHandler(registry.NewMemoryStore()))
	if err != nil || hs != nil {
		t.Fatalf("default posture %v %v", hs, err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/fed/handshake/start", strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatal(w.Code)
	}
}
