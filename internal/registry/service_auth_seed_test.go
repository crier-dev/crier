package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// seedKeyPair generates an identity and returns its private key plus the
// public half in the hex form CR_REGISTRY_SEED_IDENTITIES takes.
func seedKeyPair(t *testing.T, id string) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv, hex.EncodeToString(pub)
}

func TestParseSeedIdentities(t *testing.T) {
	_, keyA := seedKeyPair(t, "agent-a")
	_, keyB := seedKeyPair(t, "agent-b")

	ids, err := ParseSeedIdentities("agent-a=" + keyA + ", agent-b=" + keyB)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	require.Equal(t, "agent-a", ids[0].ID)
	require.Equal(t, keyA, hex.EncodeToString(ids[0].PublicKey))
	require.Equal(t, "agent-b", ids[1].ID)
	require.Equal(t, keyB, hex.EncodeToString(ids[1].PublicKey))

	// Empty (and whitespace-only) disables the declaration entirely: the
	// default state installs nothing.
	for _, raw := range []string{"", "   ", "\t"} {
		ids, err := ParseSeedIdentities(raw)
		require.NoError(t, err, raw)
		require.Empty(t, ids, raw)
	}

	for _, raw := range []string{
		"agent-a",                               // no separator
		"agent-a=",                              // no key
		"=" + keyA,                              // no id
		"agent-a=" + keyA + "=" + keyA,          // extra separator
		"agent-a=" + keyA + ",",                 // trailing empty entry
		"agent-a=" + keyA + ",,agent-b=" + keyB, // empty middle entry
		"agent a=" + keyA,                       // invalid id
		"-agent-a=" + keyA,                      // invalid id
		"agent-a=" + strings.ToUpper("zz") + keyA[2:], // non-hex key
		"agent-a=" + keyA[:len(keyA)-2],               // short key
		"agent-a=" + keyA + "00",                      // long key
		"agent-a=" + keyA + ",agent-a=" + keyB,        // duplicate identity, different key
		"agent-a=" + keyA + ",agent-a=" + keyA,        // duplicate identity, same key
	} {
		_, err := ParseSeedIdentities(raw)
		require.Error(t, err, raw)
		if err != nil {
			require.Contains(t, err.Error(), "CR_REGISTRY_SEED_IDENTITIES", raw)
		}
	}
}

// The base behaviour this row exists to keep: an empty store cannot satisfy the
// allowlist, and the refusal is the exact documented sentence.
func TestServiceDeliveryWithoutSeedIdentitiesRefusesBoot(t *testing.T) {
	policy, err := ParseServiceDeliveryAllowlist("agent-a>agent-b")
	require.NoError(t, err)
	err = ValidateServiceIdentities(NewMemoryStore(), policy)
	require.Error(t, err)
	// Map iteration order decides which identity is named first; the sentence
	// itself is the contract.
	require.Contains(t, err.Error(), "is not pre-provisioned")
	require.Contains(t, err.Error(), "agent not found")
	require.True(t, strings.Contains(err.Error(), `"agent-a"`) || strings.Contains(err.Error(), `"agent-b"`), err.Error())
}

// The declared identities provision the store, and nothing else does: an
// allowlist participant the operator did not declare keeps the boot refused.
func TestSeedIdentitiesProvisionOnlyDeclaredIdentities(t *testing.T) {
	_, keyA := seedKeyPair(t, "agent-a")
	_, keyB := seedKeyPair(t, "agent-b")
	ids, err := ParseSeedIdentities("agent-a=" + keyA + ",agent-b=" + keyB)
	require.NoError(t, err)

	store := NewMemoryStore()
	require.Error(t, ValidateServiceIdentities(store, ServiceDeliveryPolicy{"agent-b": {"agent-a": {}}}))
	require.NoError(t, ApplySeedIdentities(store, ids))

	agent, err := store.Get("agent-a")
	require.NoError(t, err)
	require.Equal(t, keyA, hex.EncodeToString(agent.PublicKey))
	require.Len(t, agent.PublicKey, ed25519.PublicKeySize)
	require.Equal(t, StatusOnline, agent.Status)
	require.NoError(t, ValidateServiceIdentities(store, ServiceDeliveryPolicy{"agent-b": {"agent-a": {}}}))

	// Seeding is not a blanket provisioning path: an allowlist identity the
	// operator never declared is still refused, with the same sentence.
	err = ValidateServiceIdentities(store, ServiceDeliveryPolicy{"agent-b": {"agent-c": {}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), `service identity "agent-c" is not pre-provisioned`)

	// A keyless declaration cannot exist (the parser requires 32 key bytes),
	// and seeding twice is a conflict rather than a silent overwrite.
	require.Error(t, ApplySeedIdentities(store, ids))
}

// A durable store keeps its own agents, so seeding it is refused outright
// instead of resurrecting/deleting rows at every boot.
func TestApplySeedIdentitiesRefusesNonMemoryStore(t *testing.T) {
	_, keyA := seedKeyPair(t, "agent-a")
	ids, err := ParseSeedIdentities("agent-a=" + keyA)
	require.NoError(t, err)

	wrapped := NewAuthorityStore(NewMemoryStore(), ServiceDeliveryPolicy{}, "relay-a")
	err = ApplySeedIdentities(wrapped, ids)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CR_REGISTRY_SEED_IDENTITIES")
	require.Contains(t, err.Error(), "in-memory registry backend")

	// An empty declaration is a no-op on ANY store: nothing is seeded, so the
	// backend restriction has nothing to refuse.
	require.NoError(t, ApplySeedIdentities(wrapped, nil))
}

// The positive end-to-end case: seeded identities + the documented allowlist
// boot a real handler, and a signed service delivery over the documented wire
// contract lands in the recipient's inbox. Nothing here registers the agents
// over HTTP — the store is provisioned by the declaration alone.
func TestSeededIdentitiesDeliverOverTheAllowlist(t *testing.T) {
	privA, keyA := seedKeyPair(t, "agent-a")
	_, keyB := seedKeyPair(t, "agent-b")
	ids, err := ParseSeedIdentities("agent-a=" + keyA + ",agent-b=" + keyB)
	require.NoError(t, err)

	base := NewMemoryStore()
	require.NoError(t, ApplySeedIdentities(base, ids))
	policy, err := ParseServiceDeliveryAllowlist("agent-a>agent-b")
	require.NoError(t, err)
	require.NoError(t, ValidateServiceIdentities(base, policy))

	secured := NewAuthorityStore(base, policy, "crier-demo")
	handler := NewHandler(secured)
	require.NoError(t, handler.SetServiceDelivery("agent-a>agent-b", "crier-demo"))
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods(http.MethodPost)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	body := `{"sender":"agent-a","payload":{"task":"dispatch"},"idempotency_key":"seed-recipe"}`
	ts := time.Now().Unix()
	digest := sha256Hex([]byte(body))
	transcript := append(agentSigPayload(http.MethodPost, "/agents/agent-b/inbox", strconv.FormatInt(ts, 10), digest), []byte("\naudience:crier-demo")...)

	post := func(headers map[string]string) (int, string) {
		req, reqErr := http.NewRequest(http.MethodPost, server.URL+"/agents/agent-b/inbox", strings.NewReader(body))
		require.NoError(t, reqErr)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, doErr := server.Client().Do(req)
		require.NoError(t, doErr)
		defer func() { _ = resp.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}

	// Unsigned first: the protected recipient must refuse it, and that must
	// not write anything.
	code, out := post(nil)
	require.Equal(t, http.StatusUnauthorized, code, out)
	depth, leased, _, err := base.Stats("agent-b")
	require.NoError(t, err)
	require.Zero(t, depth+leased)

	code, out = post(map[string]string{
		HeaderAgentID: "agent-a", HeaderAgentTS: strconv.FormatInt(ts, 10),
		HeaderAgentBodySHA256: digest, HeaderServiceAudience: "crier-demo",
		HeaderAgentSig: hex.EncodeToString(ed25519.Sign(privA, transcript)),
	})
	require.Equal(t, http.StatusCreated, code, out)
	depth, leased, _, err = base.Stats("agent-b")
	require.NoError(t, err)
	require.Equal(t, 1, depth+leased)

	// The allowlist still decides authority: a second, undeclared identity
	// cannot deliver to the same protected recipient just because it exists.
	privC, keyC := seedKeyPair(t, "agent-c")
	require.NoError(t, ApplySeedIdentities(base, []SeedIdentity{{ID: "agent-c", PublicKey: HexKey(mustDecode(t, keyC))}}))
	bodyC := strings.Replace(body, `"agent-a"`, `"agent-c"`, 1)
	bodyC = strings.Replace(bodyC, `"seed-recipe"`, `"seed-recipe-c"`, 1)
	tsC := time.Now().Unix()
	digestC := sha256Hex([]byte(bodyC))
	transcriptC := append(agentSigPayload(http.MethodPost, "/agents/agent-b/inbox", strconv.FormatInt(tsC, 10), digestC), []byte("\naudience:crier-demo")...)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/agents/agent-b/inbox", strings.NewReader(bodyC))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range map[string]string{
		HeaderAgentID: "agent-c", HeaderAgentTS: strconv.FormatInt(tsC, 10),
		HeaderAgentBodySHA256: digestC, HeaderServiceAudience: "crier-demo",
		HeaderAgentSig: hex.EncodeToString(ed25519.Sign(privC, transcriptC)),
	} {
		req.Header.Set(k, v)
	}
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	depth, leased, _, err = base.Stats("agent-b")
	require.NoError(t, err)
	require.Equal(t, 1, depth+leased)
}

func mustDecode(t *testing.T, hexKey string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(hexKey)
	require.NoError(t, err)
	return raw
}
