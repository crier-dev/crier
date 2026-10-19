package federation

import (
	"net/http"
	"strings"
)

// DeliveryAuth adds exactly one credential to the deployment gate: a linked
// instance key on a signed inbox POST. No key announcement can authenticate
// discovery, peer management, or any other route. All other requests run the
// existing bearer middleware unchanged; a claimed key can never fall back to
// a token after a failed proof. The later legacy peer-auth/policy and local
// delivery checks still run. getStore is resolved after startup composition.
func DeliveryAuth(existing func(http.Handler) http.Handler, getStore func() *PeerStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		legacy := existing(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			store := getStore()
			if store == nil || r.Header.Get(PeerHeader) == "" || !keyedInboxRoute(r) {
				legacy.ServeHTTP(w, r)
				return
			}
			if v := store.VerifyDelivery(r); v != FedAllowed {
				RefuseDelivery(w, v)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func keyedInboxRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return len(parts) == 3 && parts[1] != "" && parts[2] == "inbox" && (parts[0] == "agents" || parts[0] == "capabilities")
}
