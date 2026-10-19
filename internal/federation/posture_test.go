package federation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/middleware"
)

func TestInboundPostureMatchesKeylessRefusal(t *testing.T) {
	for _, state := range []string{"", "pending", "linked", "revoked"} {
		for _, required := range []bool{false, true} {
			store := &PeerStore{peers: make(map[string]PeerRecord)}
			if state != "" {
				store.peers["peer"] = PeerRecord{ID: "peer", State: state}
			}
			store.SetRequirePeer(required)
			p := store.InboundPosture()
			effective := required && state != ""
			require.Equal(t, required, p.RequirePeerConfigured)
			require.Equal(t, effective, p.RequirePeerEffective)
			require.Equal(t, !effective, p.TokenOnlyAccepted)
			require.Equal(t, len(store.List()), p.Peers)
			req := httptest.NewRequest("POST", "/agents/target/inbox", nil)
			req.Header.Set("Authorization", "Bearer deployment")
			w := httptest.NewRecorder()
			DeliveryAuth(middleware.Auth("deployment"), func() *PeerStore { return store })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(201)
			})).ServeHTTP(w, req)
			require.Equal(t, effective, w.Code == http.StatusForbidden)
		}
	}
	var disabled *PeerStore
	require.Equal(t, InboundPosture{TokenOnlyAccepted: true}, disabled.InboundPosture())
}

func TestInboundPostureTracksLiveChanges(t *testing.T) {
	s := &PeerStore{peers: make(map[string]PeerRecord)}
	s.SetRequirePeer(true)
	require.False(t, s.InboundPosture().RequirePeerEffective)
	s.peers["first"] = PeerRecord{ID: "first", State: "pending"}
	require.True(t, s.InboundPosture().RequirePeerEffective)
	s.SetRequirePeer(false)
	require.False(t, s.InboundPosture().RequirePeerEffective)
	s.SetRequirePeer(true)
	delete(s.peers, "first")
	require.False(t, s.InboundPosture().RequirePeerEffective)
	require.True(t, s.InboundPosture().TokenOnlyAccepted)
}
