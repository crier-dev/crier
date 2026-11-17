package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// seedBootEnv is an environment overlay for config.LoadFrom, so several boot
// postures can be composed in ONE test process without touching process env
// (the same seam cmd/server/main.go's runWithEnv uses, INT-CI-014).
func seedBootEnv(vals map[string]string) config.Getenv {
	return func(key string) string { return vals[key] }
}

// seedBootIdentities runs the production boot sequence for the registry store:
// the CR_REGISTRY_SEED_IDENTITIES declaration is applied FIRST, then the
// service-delivery policy is validated against the resulting store. It mirrors
// runWithEnv (cmd/server/main.go) exactly; the two errors are kept separate so a
// test can tell a seed refusal from a delivery-posture refusal.
func seedBootIdentities(cfg config.Config, store registry.Store) (registry.Store, *registry.Handler, error) {
	seeded, err := applyRegistrySeedIdentities(cfg, store)
	if err != nil {
		return nil, nil, err
	}
	return composeRegistryDelivery(cfg, seeded)
}

func seedBootKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv, hex.EncodeToString(pub)
}

// The positive direction: the documented dev combination — allowlist +
// audience + the operator's seed declaration — boots on the DEFAULT in-memory
// backend, and a signed service delivery over the allowlist succeeds.
func TestSeedIdentitiesBootTheDocumentedDevCombination(t *testing.T) {
	privA, keyA := seedBootKey(t)
	_, keyB := seedBootKey(t)
	cfg, err := config.LoadFrom(seedBootEnv(map[string]string{
		"CR_SERVICE_DELIVERY_ALLOWLIST": "agent-a>agent-b",
		"CR_SERVICE_DELIVERY_AUDIENCE":  "crier-demo",
		"CR_REGISTRY_SEED_IDENTITIES":   "agent-a=" + keyA + ",agent-b=" + keyB,
		"CR_REGISTRY_BACKEND":           registryBackendMemory,
		"CR_REQUIRE_AGENT_SIG":          "true",
	}))
	require.NoError(t, err)
	require.Equal(t, registryBackendMemory, cfg.ResolvedRegistryBackend())

	store, handler, err := seedBootIdentities(cfg, registry.NewMemoryStore())
	require.NoError(t, err, "seeding the declared identities must let the allowlist boot on the memory backend")
	require.NotNil(t, handler)
	require.IsType(t, &registry.AuthorityStore{}, store, "a nonempty allowlist still wraps the store in the policy-enforcing one")

	// Both declared identities are real rows with real keys, and the guard
	// passes on those rows — not by being skipped.
	for id, want := range map[string]string{"agent-a": keyA, "agent-b": keyB} {
		agent, getErr := store.Get(id)
		require.NoError(t, getErr, id)
		require.Equal(t, want, hex.EncodeToString(agent.PublicKey), id)
	}

	router := mux.NewRouter()
	registerRegistryRoutes(router, handler)
	body := `{"sender":"agent-a","payload":{"task":"dispatch"},"idempotency_key":"boot-recipe"}`
	ts := time.Now().Unix()
	digest := sha256.Sum256([]byte(body))
	digestHex := hex.EncodeToString(digest[:])
	transcript := []byte("POST\n/agents/agent-b/inbox\n" + strconv.FormatInt(ts, 10) + "\nsha256:" + digestHex + "\naudience:crier-demo")
	req := httptest.NewRequest(http.MethodPost, "/agents/agent-b/inbox", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(registry.HeaderAgentID, "agent-a")
	req.Header.Set(registry.HeaderAgentTS, strconv.FormatInt(ts, 10))
	req.Header.Set(registry.HeaderAgentBodySHA256, digestHex)
	req.Header.Set(registry.HeaderServiceAudience, "crier-demo")
	req.Header.Set(registry.HeaderAgentSig, hex.EncodeToString(ed25519.Sign(privA, transcript)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	depth, leased, _, err := store.Stats("agent-b")
	require.NoError(t, err)
	require.Equal(t, 1, depth+leased)
}

// The negative direction, unchanged from the base commit: without the
// declaration the SAME boot refuses with the SAME sentence, because the memory
// store starts empty and the guard is not relaxed.
func TestSeedIdentitiesWithoutDeclarationStillRefusesBoot(t *testing.T) {
	_, keyA := seedBootKey(t)
	_, keyB := seedBootKey(t)
	for _, tc := range []struct {
		name string
		seed string
	}{
		{"no_declaration", ""},
		{"partial_declaration", "agent-b=" + keyB},
		{"declaration_of_an_unrelated_identity", "agent-c=" + keyA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"CR_SERVICE_DELIVERY_ALLOWLIST": "agent-a>agent-b",
				"CR_SERVICE_DELIVERY_AUDIENCE":  "crier-demo",
				"CR_REGISTRY_BACKEND":           registryBackendMemory,
			}
			if tc.seed != "" {
				env["CR_REGISTRY_SEED_IDENTITIES"] = tc.seed
			}
			cfg, err := config.LoadFrom(seedBootEnv(env))
			require.NoError(t, err)

			store, handler, err := seedBootIdentities(cfg, registry.NewMemoryStore())
			require.Error(t, err)
			require.Nil(t, handler)
			require.Nil(t, store)
			// The exact pre-existing sentence, still produced by
			// ValidateServiceIdentities — the declaration must not have
			// turned the guard advisory.
			require.Contains(t, err.Error(), "is not pre-provisioned")
			require.Contains(t, err.Error(), "agent not found")
		})
	}
}

// A durable backend keeps its own agents, so a seed declaration is refused
// there instead of silently re-creating rows at every boot.
func TestSeedIdentitiesRefuseDurableBackend(t *testing.T) {
	_, keyA := seedBootKey(t)
	cfg, err := config.LoadFrom(seedBootEnv(map[string]string{
		"CR_REGISTRY_SEED_IDENTITIES": "agent-a=" + keyA,
	}))
	require.NoError(t, err)

	durable := registry.NewAuthorityStore(registry.NewMemoryStore(), registry.ServiceDeliveryPolicy{}, "crier-demo")
	_, _, err = seedBootIdentities(cfg, durable)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CR_REGISTRY_SEED_IDENTITIES")
	require.Contains(t, err.Error(), "in-memory registry backend")
}

// A malformed declaration fails the boot at config load, before any store is
// opened — the same place the allowlist grammar is enforced.
func TestSeedIdentitiesMalformedDeclarationFailsLoad(t *testing.T) {
	_, err := config.LoadFrom(seedBootEnv(map[string]string{
		"CR_REGISTRY_SEED_IDENTITIES": "agent-a=not-a-key",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "CR_REGISTRY_SEED_IDENTITIES")
}
