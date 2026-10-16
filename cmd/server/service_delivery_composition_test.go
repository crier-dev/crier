package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func TestProductionServiceDeliveryCompositionAndRoutes(t *testing.T) {
	base := registry.NewMemoryStore()
	keys := map[string]ed25519.PrivateKey{}
	for _, id := range []string{"scheduler", "router", "ordinary"} {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		keys[id] = priv
		require.NoError(t, base.Register(&registry.Agent{ID: id, PublicKey: registry.HexKey(pub)}))
	}
	cfg := config.Config{ServiceDeliveryAllowlist: "scheduler>router", ServiceDeliveryAudience: "relay-a"}
	store, handler, err := composeRegistryDelivery(cfg, base)
	require.NoError(t, err)
	require.IsType(t, &registry.AuthorityStore{}, store)
	router := mux.NewRouter()
	registerRegistryRoutes(router, handler)
	router.HandleFunc("/status", newStatusHandler(cfg, registryBackendMemory)).Methods(http.MethodGet)

	post := func(target, body string, signed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
		if signed {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			digest := sha256.Sum256([]byte(body))
			digestHex := hex.EncodeToString(digest[:])
			transcript := []byte(http.MethodPost + "\n" + req.URL.Path + "\n" + ts + "\nsha256:" + digestHex + "\naudience:relay-a")
			req.Header.Set(registry.HeaderAgentID, "scheduler")
			req.Header.Set(registry.HeaderAgentTS, ts)
			req.Header.Set(registry.HeaderAgentBodySHA256, digestHex)
			req.Header.Set(registry.HeaderServiceAudience, "relay-a")
			req.Header.Set(registry.HeaderAgentSig, hex.EncodeToString(ed25519.Sign(keys["scheduler"], transcript)))
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	valid := `{"sender":"scheduler","payload":{"task":1},"idempotency_key":"production"}`
	require.Equal(t, http.StatusCreated, post("/agents/router/inbox", valid, true).Code)
	unsigned := post("/agents/router/inbox", `{"sender":"scheduler","payload":{},"idempotency_key":"unsigned"}`, false)
	require.Equal(t, http.StatusUnauthorized, unsigned.Code, unsigned.Body.String())
	depth, _, _, err := base.Stats("router")
	require.NoError(t, err)
	require.Equal(t, 1, depth, "unsigned request must not write")
	require.Equal(t, http.StatusCreated, post("/agents/ordinary/inbox", `{"payload":{"legacy":true}}`, false).Code)

	statusReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	status := httptest.NewRecorder()
	router.ServeHTTP(status, statusReq)
	require.Equal(t, http.StatusOK, status.Code)
	var posture map[string]any
	require.NoError(t, json.Unmarshal(status.Body.Bytes(), &posture))
	require.Equal(t, true, posture["service_delivery_auth_enabled"])
	require.Equal(t, true, posture["service_delivery_audience_present"])
	require.EqualValues(t, 1, posture["service_delivery_protected_recipient_count"])
	require.EqualValues(t, 1, posture["service_delivery_directed_pair_count"])
	require.NotContains(t, status.Body.String(), "scheduler")
	require.NotContains(t, status.Body.String(), "router")
}

func TestProductionServiceDeliveryCompositionFailsClosed(t *testing.T) {
	keyed := func(ids ...string) *registry.MemoryStore {
		s := registry.NewMemoryStore()
		for _, id := range ids {
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			require.NoError(t, s.Register(&registry.Agent{ID: id, PublicKey: registry.HexKey(pub)}))
		}
		return s
	}
	for _, tc := range []struct {
		name, allow, audience string
		store                 registry.Store
	}{
		{"invalid_allowlist", "scheduler-* > router", "relay-a", keyed("scheduler", "router")},
		{"missing_audience", "scheduler>router", "", keyed("scheduler", "router")},
		{"missing_identity", "scheduler>router", "relay-a", keyed("router")},
		{"keyless_identity", "scheduler>router", "relay-a", func() registry.Store {
			s := keyed("router")
			require.NoError(t, s.Register(&registry.Agent{ID: "scheduler"}))
			return s
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := composeRegistryDelivery(config.Config{ServiceDeliveryAllowlist: tc.allow, ServiceDeliveryAudience: tc.audience}, tc.store)
			require.Error(t, err)
		})
	}
}
