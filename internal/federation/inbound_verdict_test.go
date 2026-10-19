package federation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/middleware"
)

func TestInbound371EveryDoorHasReceiptAndAudit(t *testing.T) {
	h, key, _ := handshakeFixture(t)
	// Link through the same store transition the handshake uses.
	_, err := h.Store.link("a", KeyID(key.Public().(ed25519.PublicKey)))
	require.NoError(t, err)
	c := NewClient(nil, 0, "")
	c.SetSelfKey(key)
	c.SetPeerRecords(h.Store, "a")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, tc := range []struct {
		name, peer, sig, bearer string
		hop                     bool
		want                    Verdict
		status                  int
	}{
		{name: "signed", peer: "a", sig: "valid", want: FedAllowed, status: 201},
		{name: "token_only", bearer: "deployment", hop: true, want: FedTokenOnly, status: 201},
		{name: "no_header", bearer: "deployment", want: FedTokenOnly, status: 201},
		{name: "no_credential", want: FedTokenOnly, status: 401},
		{name: "missing_signature", peer: "a", bearer: "deployment", want: FedBadSignature, status: 401},
		{name: "bad_signature", peer: "a", sig: "bad", bearer: "deployment", want: FedBadSignature, status: 401},
		{name: "unknown", peer: "unknown", bearer: "deployment", want: FedNoLink, status: 403},
		{name: "revoked", peer: "a", sig: "valid", want: FedPeerRevoked, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			body := []byte(`{"payload":{}}`)
			r := httptest.NewRequest("POST", "/agents/target/inbox", bytes.NewReader(body))
			if tc.sig != "" {
				SignFedRequest(r, key, time.Now())
				require.NoError(t, c.preparePeerForward(r, Link{URL: "https://a.example"}, body))
			}
			r.Header.Set(PeerHeader, tc.peer)
			if tc.sig == "bad" {
				r.Header.Set("X-Fed-Body-Sig", hex.EncodeToString(make([]byte, ed25519.SignatureSize)))
			}
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.hop {
				r.Header.Set(HopHeader, "1")
			}
			r.Header.Set("X-Crier-Namespace-Token", "must-not-log")
			if tc.name == "revoked" {
				state := "revoked"
				_, err := h.Store.Patch("a", PeerPatch{State: &state})
				require.NoError(t, err)
			}
			calls := 0
			gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return h.Store })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":"receipt"}`))
			}))
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, string(tc.want), w.Header().Get(VerdictHeader))
			var receipt map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &receipt))
			require.Equal(t, string(tc.want), receipt["verdict"])
			require.Equal(t, 1, strings.Count(logs.String(), `"msg":"federation inbound delivery"`), logs.String())
			require.Contains(t, logs.String(), `"verdict":"`+string(tc.want)+`"`)
			require.NotContains(t, logs.String(), "must-not-log")
			require.NotContains(t, logs.String(), "deployment")
			if tc.status == 201 {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
			t.Logf("door=%s status=%d receipt=%s audit=%s", tc.name, w.Code, w.Body.String(), logs.String())
		})
	}
	t.Run("panic_preserves_outer_recovery", func(t *testing.T) {
		logs.Reset()
		r := httptest.NewRequest("POST", "/agents/target/inbox", nil)
		r.Header.Set("Authorization", "Bearer deployment")
		gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return h.Store })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"buffered_success"}`))
			panic("downstream panic")
		}))
		w := httptest.NewRecorder()
		func() {
			defer func() {
				if recover() != nil {
					w.WriteHeader(500)
					_, _ = w.Write([]byte(`{"error":"internal server error"}`))
				}
			}()
			gate.ServeHTTP(w, r)
		}()
		require.Equal(t, 500, w.Code)
		require.NotContains(t, w.Body.String(), "buffered_success")
		// DF-CRIER-394: the 500 the client sees is the recovery middleware's
		// response, not this receipt's, so the audit line may not carry a status
		// for it. It must say the receipt was abandoned instead - an operator or
		// an alert reading this line must not see a classified delivery.
		require.NotContains(t, logs.String(), `"status":`, "no status is claimed for a receipt that was never sent")
		require.Contains(t, logs.String(), `"delivered":false`)
		require.Contains(t, logs.String(), `"abandoned":true`)
		require.Equal(t, 1, strings.Count(logs.String(), `"msg":"federation inbound delivery"`))
	})
}

func TestInbound371StrictPolicyAndEmptyStoreCompatibility(t *testing.T) {
	h, _, _ := handshakeFixture(t)
	setter, ok := any(h.Store).(interface{ SetRequirePeer(bool) })
	require.True(t, ok, "peer store lacks the opt-in inbound key requirement")
	setter.SetRequirePeer(true)
	for _, path := range []string{"/agents/target/inbox", "/capabilities/tool/inbox"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"payload":{}}`))
		r.Header.Set("Authorization", "Bearer deployment")
		w := httptest.NewRecorder()
		gate := DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return h.Store })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("token bypassed strict policy") }))
		gate.ServeHTTP(w, r)
		require.Equal(t, 403, w.Code)
		require.Contains(t, w.Body.String(), string(FedNoLink))
	}
	empty, err := OpenPeerStore(t.TempDir() + "/empty.json")
	require.NoError(t, err)
	any(empty).(interface{ SetRequirePeer(bool) }).SetRequirePeer(true)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	for _, store := range []*PeerStore{nil, empty} {
		for _, token := range []string{"deployment", "wrong", ""} {
			logs.Reset()
			r := httptest.NewRequest("POST", "/agents/target/inbox", nil)
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":"unchanged"}`))
			})
			w, want := httptest.NewRecorder(), httptest.NewRecorder()
			middleware.Auth("deployment")(next).ServeHTTP(want, r)
			DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return store })(next).ServeHTTP(w, r)
			require.Equal(t, want.Code, w.Code)
			require.Equal(t, want.Body.String(), w.Body.String())
			require.Equal(t, string(FedTokenOnly), w.Header().Get(VerdictHeader))
			require.Contains(t, logs.String(), `"msg":"federation inbound delivery"`)
		}
	}
}
