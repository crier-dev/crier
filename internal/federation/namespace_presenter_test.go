package federation_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
)

// The token is deliberately added AFTER peer B's signatures are made. This
// reproduces the unsigned-header contract, not a proposed anti-replay guarantee.
func TestNamespace396BearerReplayAttributesPresenter(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	const token = "namespace-396-test-credential"
	policies, err := namespace.Parse(`{"namespaces":[{"name":"private","auth":"token","token_ref":"env:REALM_TOKEN"},{"name":"default","auth":"token","token_ref":"env:REALM_TOKEN"},{"name":"shared"}]}`, func(string) (string, bool) { return token, true })
	require.NoError(t, err)
	store := registry.NewMemoryStore()
	for _, name := range []string{"private", "default", "shared"} {
		require.NoError(t, store.Register(&registry.Agent{ID: name, Namespace: name, Capabilities: []string{name}}))
	}
	handler := registry.NewHandler(store)
	handler.SetNamespacePolicies(policies)
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	router.HandleFunc("/capabilities/{capability}/inbox", handler.HandleDeliverByCapability).Methods("POST")
	peers, err := federation.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	require.NoError(t, err)
	handler.SetPeerRecords(peers)
	gate := federation.DeliveryAuth(middleware.Auth("deployment"), func() *federation.PeerStore { return peers })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mutate_peer") != "" {
			// A downstream mutable header must never rewrite the verified
			// identity snapshot that the namespace audit consumes.
			r.Header.Set(federation.PeerHeader, "peer-a")
		}
		router.ServeHTTP(w, r)
	}))
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, receiverKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, err = peers.Create(federation.PeerRecord{ID: "peer-b", PublicKey: hex.EncodeToString(pub), URL: "https://b.example", Direction: "both"})
	require.NoError(t, err)
	hs, err := federation.NewHandshakeService(peers, "receiver", receiverKey)
	require.NoError(t, err)
	proof, err := hs.Start(federation.HandshakeStart{InstanceID: "peer-b", PublicKey: hex.EncodeToString(pub), KeyID: federation.KeyID(pub), Nonce: strings.Repeat("ab", 32), Timestamp: time.Now().Unix()})
	require.NoError(t, err)
	proof.InitiatorSignature = hex.EncodeToString(ed25519.Sign(key, proof.Transcript.Payload()))
	_, err = hs.Finish(proof)
	require.NoError(t, err)

	for _, tc := range []struct {
		name, target, path, presented, peer string
		signed, authorized                  bool
		status                              int
	}{
		{"peer_b_replays_token", "private", "/agents/private/inbox", token, "peer-b", true, true, 201},
		{"verified_snapshot_not_mutable_header", "private", "/agents/private/inbox?mutate_peer=1", token, "peer-b", true, true, 201},
		{"default_realm", "default", "/agents/default/inbox", token, "peer-b", true, true, 201},
		{"capability", "private", "/capabilities/private/inbox", token, "peer-b", true, true, 201},
		{"wrong_token", "private", "/agents/private/inbox", "wrong", "peer-b", true, false, 401},
		{"missing_token", "private", "/agents/private/inbox", "", "peer-b", true, false, 401},
		{"shared_not_token_authorized", "shared", "/agents/shared/inbox", token, "peer-b", true, false, 201},
		{"legacy_unattributed", "private", "/agents/private/inbox", token, "", false, true, 201},
		{"spoofed_peer", "private", "/agents/private/inbox", token, "peer-b", false, false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"sender":"claimed-agent-a","payload":{"text":"delivery"}}`)
			r := httptest.NewRequest("POST", tc.path, bytes.NewReader(body))
			if tc.signed {
				federation.SignFedRequest(r, key, time.Now())
				r.Header.Set(federation.PeerHeader, "peer-b")
				r.Header.Set("X-Fed-Key-Id", federation.KeyID(pub))
				prefix := "crier-fed-delivery-v1\npeer-b\n" + federation.KeyID(pub) + "\n" + string(federation.FedAuthPayload(r.Method, r.URL.Path, r.Header.Get(federation.FedAuthTsHeader))) + "\n" + strconv.Itoa(len(body)) + "\n"
				r.Header.Set("X-Fed-Body-Sig", hex.EncodeToString(ed25519.Sign(key, append([]byte(prefix), body...))))
			}
			r.Header.Set(federation.PeerHeader, tc.peer)
			r.Header.Set(namespace.HeaderNamespaceToken, tc.presented)
			if !tc.signed {
				r.Header.Set("Authorization", "Bearer deployment")
			}
			logs.Reset()
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.NotContains(t, logs.String(), token)
			require.NotContains(t, w.Body.String(), token)
			var audits []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &record))
				if record["msg"] == "namespace delivery authorized" {
					audits = append(audits, record)
				}
			}
			if !tc.authorized {
				require.Empty(t, audits)
				return
			}
			entries, _, err := store.Retrieve(tc.target, 0, 100)
			require.NoError(t, err)
			require.NotEmpty(t, entries, "unsigned namespace token must authorize delivery with the existing ingress credential")
			raw, err := json.Marshal(entries)
			require.NoError(t, err)
			require.NotContains(t, string(raw), token)
			require.Len(t, audits, 1, "namespace-authorized delivery has no presenter audit (unsigned token accepted)")
			audit := audits[0]
			require.Equal(t, tc.peer, audit["presenter_peer"])
			identity := "verified_peer"
			if !tc.signed {
				identity = "unattributed"
			}
			require.Equal(t, identity, audit["presenter_identity"])
			require.Equal(t, "bearer_per_hop", audit["namespace_auth"])
			require.Equal(t, tc.target, audit["namespace"])
			require.Equal(t, tc.target, audit["target"])
			require.NotEmpty(t, audit["message_id"])
			t.Logf("unsigned header accepted; presenter_peer=%q presenter_identity=%s", tc.peer, identity)
		})
	}
	t.Run("session_dispatch_has_no_invented_peer", func(t *testing.T) {
		logs.Reset()
		target, err := store.Get("private")
		require.NoError(t, err)
		result, err := handler.Dispatch(t.Context(), target, registry.DeliveryRequest{
			Entry:          &registry.InboxEntry{ID: "session-396", AgentID: "private", Payload: json.RawMessage(`{"text":"in-process"}`)},
			NamespaceToken: token,
		})
		require.NoError(t, err)
		require.Equal(t, 201, result.StatusCode)
		require.Contains(t, logs.String(), `"msg":"namespace delivery authorized"`)
		require.Contains(t, logs.String(), `"message_id":"session-396"`)
		require.Contains(t, logs.String(), `"presenter_peer":""`)
		require.Contains(t, logs.String(), `"presenter_identity":"unattributed"`)
		require.Equal(t, 1, strings.Count(logs.String(), `"msg":"namespace delivery authorized"`))
		require.NotContains(t, logs.String(), token)
	})
}
