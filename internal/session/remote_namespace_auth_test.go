package session

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
)

func TestRemote369NamespaceCredentialAndTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		delivered   bool
		signed      bool
	}{
		{name: "legacy_missing"}, {name: "legacy_wrong", token: "wrong"}, {name: "legacy_correct", token: "realm-secret", delivered: true},
		{name: "signed_missing", signed: true}, {name: "signed_wrong", signed: true, token: "wrong"}, {name: "signed_correct", signed: true, token: "realm-secret", delivered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			policies, err := namespace.Parse(`{"namespaces":[{"name":"private","auth":"token","token_ref":"env:REALM_TOKEN"}]}`, func(key string) (string, bool) { return "realm-secret", key == "REALM_TOKEN" })
			require.NoError(t, err)
			sourcePolicies, err := namespace.Parse(`{"namespaces":[{"name":"private"}]}`, nil)
			require.NoError(t, err)
			destStore := registry.NewMemoryStore()
			require.NoError(t, destStore.Register(&registry.Agent{ID: "target", Namespace: "private"}))
			dest := registry.NewHandler(destStore)
			dest.SetNamespacePolicies(policies)
			router := mux.NewRouter()
			router.HandleFunc("/agents/{id}/inbox", dest.HandleDeliver).Methods("POST")
			// Observe wire metadata independently of the destination's CheckAuth.
			var wireBody []byte
			var wireToken string
			var peerRecords *federation.PeerStore
			protected := federation.DeliveryAuth(middleware.Auth("deployment"), func() *federation.PeerStore { return peerRecords })(router)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wireToken = r.Header.Get(namespace.HeaderNamespaceToken)
				var buf bytes.Buffer
				_, err := buf.ReadFrom(r.Body)
				require.NoError(t, err)
				wireBody = buf.Bytes()
				r.Body = io.NopCloser(bytes.NewReader(wireBody))
				protected.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			a := newInstance(t)
			a.handler.opts.Namespaces = sourcePolicies
			pp := federation.PeerPolicies{"b": {Peer: "b", URL: srv.URL, NamespacesAllow: []string{"private"}}}
			a.armPeerPolicy(pp)
			client := federation.NewClient(nil, 0, "deployment")
			client.SetPeerPolicies(pp)
			if tc.signed {
				peerRecords = linkNamespaceInstances369(t, client, a.srv.URL, srv.URL)
				dest.SetPeerRecords(peerRecords)
			}
			a.handler.SetFedClient(client)
			status, body := a.do("POST", "/sessions", `{"id":"private-room","namespace":"private","created_by":{"agent":"sender"}}`, nil)
			require.Equal(t, 201, status, body)
			status, body = a.do("POST", "/sessions/private-room/participants", `{"member_type":"agent","member_id":"remote:b:target"}`, map[string]string{namespace.HeaderNamespace: "private"})
			require.Equal(t, 201, status, body)
			headers := map[string]string{namespace.HeaderNamespaceToken: tc.token, namespace.HeaderNamespace: "private"}
			status, body = a.do("POST", "/sessions/private-room/messages", `{"sender":"sender","payload":{"text":"crossing"}}`, headers)
			require.Equal(t, 201, status, body)
			var sent transcriptMessage
			require.NoError(t, json.Unmarshal([]byte(body), &sent))
			require.Len(t, sent.Outcomes, 1)
			require.Equal(t, tc.token, wireToken, "namespace credential was dropped at the link")
			require.NotContains(t, string(wireBody), "realm-secret")
			require.NotContains(t, string(wireBody), "NamespaceToken")
			entries, _, err := destStore.Retrieve("target", 0, 10)
			require.NoError(t, err)
			if tc.delivered {
				require.Len(t, entries, 1)
				require.Equal(t, "accepted", sent.Outcomes[0].Outcome)
			} else {
				require.Empty(t, entries)
				require.Equal(t, "refused", sent.Outcomes[0].Outcome)
				require.Equal(t, "NAMESPACE_UNAUTHORIZED", sent.Outcomes[0].Code)
				require.Contains(t, sent.Outcomes[0].Detail, "NAMESPACE_UNAUTHORIZED")
			}
			status, transcript := a.do("GET", "/sessions/private-room/messages", "", map[string]string{namespace.HeaderNamespace: "private"})
			require.Equal(t, 200, status)
			if !tc.delivered {
				require.Contains(t, transcript, "NAMESPACE_UNAUTHORIZED")
			}
			require.NotContains(t, transcript, "realm-secret")
			require.NotContains(t, logs.String(), "realm-secret")
			// Inspect the actual transcript bytes, not merely the API projection.
			raw, err := os.ReadFile(filepath.Join(a.handler.store.(*JSONLStore).Root(), "private-room.jsonl"))
			require.NoError(t, err)
			require.NotContains(t, string(raw), "realm-secret")
			if !tc.delivered {
				require.Contains(t, string(raw), "NAMESPACE_UNAUTHORIZED")
			}
			t.Logf("token=%s transcript=%s", tc.name, transcript)
			// Negative control: the same envelope writes if the destination realm
			// gate is deliberately unwired. This is NOT a claim about old HEAD:
			// old HEAD already checks auth; it dropped the token and named outcome.
			dest.SetNamespacePolicies(nil)
			status, control := a.do("POST", "/sessions/private-room/messages", `{"sender":"sender","payload":{"text":"bypass control"}}`, map[string]string{namespace.HeaderNamespace: "private"})
			require.Equal(t, 201, status, control)
			entries, _, err = destStore.Retrieve("target", 0, 10)
			require.NoError(t, err)
			require.NotEmpty(t, entries, "unwired auth control must write")
			var controlSent transcriptMessage
			require.NoError(t, json.Unmarshal([]byte(control), &controlSent))
			require.Equal(t, "accepted", controlSent.Outcomes[0].Outcome)
		})
	}
}

// Complete both nonce exchanges in-process using the actual handshake service;
// no prelinked JSON fixture can accidentally wave the identity gate through.
func linkNamespaceInstances369(t *testing.T, c *federation.Client, aURL, bURL string) *federation.PeerStore {
	t.Helper()
	apub, akey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	bpub, bkey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	as, err := federation.OpenPeerStore(filepath.Join(t.TempDir(), "a.json"))
	require.NoError(t, err)
	bs, err := federation.OpenPeerStore(filepath.Join(t.TempDir(), "b.json"))
	require.NoError(t, err)
	_, err = as.Create(federation.PeerRecord{ID: "b", PublicKey: hex.EncodeToString(bpub), URL: bURL, Direction: "both"})
	require.NoError(t, err)
	_, err = bs.Create(federation.PeerRecord{ID: "a", PublicKey: hex.EncodeToString(apub), URL: aURL, Direction: "both"})
	require.NoError(t, err)
	ah, err := federation.NewHandshakeService(as, "a", akey)
	require.NoError(t, err)
	bh, err := federation.NewHandshakeService(bs, "b", bkey)
	require.NoError(t, err)
	for _, pair := range []struct {
		id        string
		key       ed25519.PrivateKey
		responder *federation.HandshakeService
		pub       ed25519.PublicKey
	}{
		{"a", akey, bh, bpub}, {"b", bkey, ah, apub},
	} {
		nonce := make([]byte, 32)
		_, err := rand.Read(nonce)
		require.NoError(t, err)
		pub := pair.key.Public().(ed25519.PublicKey)
		proof, err := pair.responder.Start(federation.HandshakeStart{InstanceID: pair.id, PublicKey: hex.EncodeToString(pub), KeyID: federation.KeyID(pub), Nonce: hex.EncodeToString(nonce), Timestamp: time.Now().Unix()})
		require.NoError(t, err)
		sig, err := hex.DecodeString(proof.ResponderSignature)
		require.NoError(t, err)
		require.True(t, ed25519.Verify(pair.pub, proof.Transcript.Payload(), sig))
		proof.InitiatorSignature = hex.EncodeToString(ed25519.Sign(pair.key, proof.Transcript.Payload()))
		_, err = pair.responder.Finish(proof)
		require.NoError(t, err)
	}
	c.SetPeerRecords(as, "a")
	c.SetSelfKey(akey)
	return bs
}
