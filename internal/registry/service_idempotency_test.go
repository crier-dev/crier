package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

const serviceTooLargeResponse = "{\"error\":\"REQUEST_BODY_TOO_LARGE\"}\n"

func strictServiceHandler(t *testing.T) (*Handler, *MemoryStore, map[string]ed25519.PrivateKey) {
	t.Helper()
	store := NewMemoryStore()
	keys := map[string]ed25519.PrivateKey{}
	for _, id := range []string{"scheduler", "backup", "router", "ordinary"} {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		keys[id] = priv
		require.NoError(t, store.Register(&Agent{ID: id, PublicKey: HexKey(pub)}))
	}
	h := NewHandler(store)
	require.NoError(t, h.SetServiceDelivery("scheduler>router,backup>router", "relay-a"))
	return h, store, keys
}

func strictServicePost(t *testing.T, h *Handler, key ed25519.PrivateKey, sender, body, audience string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/agents/router/inbox"
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for name, value := range serviceHeaders(key, sender, http.MethodPost, path, body, time.Now()) {
		req.Header.Set(name, value)
	}
	if audience != "" {
		req.Header.Set(HeaderServiceAudience, audience)
		ts := req.Header.Get(HeaderAgentTS)
		digest := req.Header.Get(HeaderAgentBodySHA256)
		transcript := append(agentSigPayload(http.MethodPost, path, ts, digest), []byte("\naudience:"+audience)...)
		req.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(key, transcript)))
	}
	req = mux.SetURLVars(req, map[string]string{"id": "router"})
	w := httptest.NewRecorder()
	h.HandleDeliver(w, req)
	return w
}

func TestServiceAuthorityStoreAndDurableMemoryReplay(t *testing.T) {
	t.Parallel()
	h, store, keys := strictServiceHandler(t)
	secured := h.store.(*AuthorityStore)
	require.ErrorIs(t, secured.Deliver("router", &InboxEntry{Payload: json.RawMessage(`{}`)}), ErrServiceAuthorityRequired)

	h.SetGlobalRateLimit(1)
	body := `{"sender":"scheduler","payload":{"task":1},"idempotency_key":"same"}`
	first := strictServicePost(t, h, keys["scheduler"], "scheduler", body, "relay-a")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	replay := strictServicePost(t, h, keys["scheduler"], "scheduler", body, "relay-a")
	require.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	require.Equal(t, first.Body.String(), replay.Body.String())
	depth, _, _, err := store.Stats("router")
	require.NoError(t, err)
	require.Equal(t, 1, depth)

	changed := strictServicePost(t, h, keys["scheduler"], "scheduler", `{"sender":"scheduler","payload":{"task":2},"idempotency_key":"same"}`, "relay-a")
	require.Equal(t, http.StatusConflict, changed.Code, changed.Body.String())

	// Sender is part of the namespace. This reaches the budget (and is shed),
	// proving the scheduler replay above consumed no fresh token.
	otherBody := `{"sender":"backup","payload":{"task":1},"idempotency_key":"same"}`
	other := strictServicePost(t, h, keys["backup"], "backup", otherBody, "relay-a")
	require.Equal(t, http.StatusTooManyRequests, other.Code, other.Body.String())
}

func TestServiceMissingKeyAndAudienceRefuseBeforeEffects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, audience string
		status               int
	}{
		{"missing_key", `{"sender":"scheduler","payload":{}}`, "relay-a", http.StatusBadRequest},
		{"missing_audience", `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, "", http.StatusUnauthorized},
		{"wrong_audience", `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, "relay-b", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, keys := strictServiceHandler(t)
			h.SetGlobalRateLimit(1)
			got := strictServicePost(t, h, keys["scheduler"], "scheduler", tc.body, tc.audience)
			require.Equal(t, tc.status, got.Code, got.Body.String())
			depth, _, _, err := store.Stats("router")
			require.NoError(t, err)
			require.Zero(t, depth)
			valid := strictServicePost(t, h, keys["scheduler"], "scheduler", `{"sender":"scheduler","payload":{},"idempotency_key":"valid"}`, "relay-a")
			require.Equal(t, http.StatusCreated, valid.Code, valid.Body.String())
		})
	}
}

func TestProtectedServiceBodyLimitPrecedesAuthorizationReplayAndBudget(t *testing.T) {
	t.Parallel()
	const limit = 192
	for _, chunked := range []bool{false, true} {
		t.Run(map[bool]string{false: "content_length", true: "chunked"}[chunked], func(t *testing.T) {
			h, exactStore, keys := strictServiceHandler(t)
			h.SetMaxInboxBodyBytes(limit)
			h.SetGlobalRateLimit(1)
			base := `{"sender":"scheduler","payload":{},"idempotency_key":"limit"}`
			exact := base + strings.Repeat(" ", limit-len(base))
			req := httptest.NewRequest(http.MethodPost, "/agents/router/inbox", strings.NewReader(exact))
			for name, value := range serviceHeaders(keys["scheduler"], "scheduler", http.MethodPost, req.URL.Path, exact, time.Now()) {
				req.Header.Set(name, value)
			}
			req.Header.Set(HeaderServiceAudience, "relay-a")
			ts, digest := req.Header.Get(HeaderAgentTS), req.Header.Get(HeaderAgentBodySHA256)
			req.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(keys["scheduler"], append(agentSigPayload(http.MethodPost, req.URL.Path, ts, digest), []byte("\naudience:relay-a")...))))
			req = mux.SetURLVars(req, map[string]string{"id": "router"})
			w := httptest.NewRecorder()
			h.HandleDeliver(w, req)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			exactDepth, _, _, err := exactStore.Stats("router")
			require.NoError(t, err)
			require.Equal(t, 1, exactDepth)

			// Use a fresh handler so the exact-limit control does not consume the
			// one-token budget whose ordering this refusal proves.
			h, store, keys := strictServiceHandler(t)
			h.SetMaxInboxBodyBytes(limit)
			h.SetGlobalRateLimit(1)
			oversize := exact + " "
			req = httptest.NewRequest(http.MethodPost, "/agents/router/inbox", strings.NewReader(oversize))
			if chunked {
				req.ContentLength = -1
			}
			req = mux.SetURLVars(req, map[string]string{"id": "router"})
			w = httptest.NewRecorder()
			h.HandleDeliver(w, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			require.Equal(t, serviceTooLargeResponse, w.Body.String())
			depth, leased, _, err := store.Stats("router")
			require.NoError(t, err)
			require.Zero(t, depth+leased)
			require.Empty(t, h.idempotency.records)
			require.Empty(t, h.idempotency.claims)

			valid := `{"sender":"scheduler","payload":{},"idempotency_key":"first"}`
			got := strictServicePost(t, h, keys["scheduler"], "scheduler", valid, "relay-a")
			require.Equal(t, http.StatusCreated, got.Code, got.Body.String())
			depth, _, _, err = store.Stats("router")
			require.NoError(t, err)
			require.Equal(t, 1, depth)
		})
	}
}

func TestProtectedServiceSignatureUsesDecodedPathAndExcludesQuery(t *testing.T) {
	t.Parallel()
	h, store, keys := strictServiceHandler(t)
	body := `{"sender":"scheduler","payload":{},"idempotency_key":"path"}`
	request := func(target, signedPath string, includeQuery bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		headers := serviceHeaders(keys["scheduler"], "scheduler", http.MethodPost, signedPath, body, time.Now())
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set(HeaderServiceAudience, "relay-a")
		ts, digest := req.Header.Get(HeaderAgentTS), req.Header.Get(HeaderAgentBodySHA256)
		transcriptPath := signedPath
		if includeQuery {
			transcriptPath += "?trace=changed"
		}
		transcript := append(agentSigPayload(http.MethodPost, transcriptPath, ts, digest), []byte("\naudience:relay-a")...)
		req.Header.Set(HeaderAgentSig, hex.EncodeToString(ed25519.Sign(keys["scheduler"], transcript)))
		req = mux.SetURLVars(req, map[string]string{"id": "router"})
		w := httptest.NewRecorder()
		h.HandleDeliver(w, req)
		return w
	}
	got := request("/agents/router%3Aservice/inbox?trace=changed", "/agents/router:service/inbox", false)
	// The route variable is deliberately reset to the provisioned recipient;
	// URL.Path remains decoded and is the signature transcript input.
	require.Equal(t, http.StatusCreated, got.Code, got.Body.String())

	for _, tc := range []struct {
		name, signedPath string
		query            bool
	}{
		{"encoded_path", "/agents/router%3Aservice/inbox", false},
		{"query_in_transcript", "/agents/router:service/inbox", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := request("/agents/router%3Aservice/inbox?trace=changed", tc.signedPath, tc.query)
			require.Equal(t, http.StatusUnauthorized, got.Code)
			require.Equal(t, "{\"error\":\"signature verification failed\"}\n", got.Body.String())
		})
	}
	depth, _, _, err := store.Stats("router")
	require.NoError(t, err)
	require.Equal(t, 1, depth)
}

func TestProtectedServiceTranscriptFailuresHaveExactContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, signMethod, signPath, signedBody, sentBody, audience string
		status                                                     int
		response                                                   string
	}{
		{"wrong_method", http.MethodGet, "/agents/router/inbox", `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, "relay-a", http.StatusUnauthorized, "{\"error\":\"signature verification failed\"}\n"},
		{"wrong_path", http.MethodPost, "/agents/other/inbox", `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, "relay-a", http.StatusUnauthorized, "{\"error\":\"signature verification failed\"}\n"},
		{"wrong_body", http.MethodPost, "/agents/router/inbox", `{"sender":"scheduler","payload":{"v":1},"idempotency_key":"k"}`, `{"sender":"scheduler","payload":{"v":2},"idempotency_key":"k"}`, "relay-a", http.StatusUnauthorized, "{\"error\":\"" + agentSigBodyDigestMismatchError + "\"}\n"},
		{"wrong_audience", http.MethodPost, "/agents/router/inbox", `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, `{"sender":"scheduler","payload":{},"idempotency_key":"k"}`, "relay-b", http.StatusUnauthorized, "{\"error\":\"service delivery audience missing or mismatched\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, keys := strictServiceHandler(t)
			headers := serviceHeaders(keys["scheduler"], "scheduler", tc.signMethod, tc.signPath, tc.signedBody, time.Now())
			headers[HeaderServiceAudience] = tc.audience
			ts, digest := headers[HeaderAgentTS], headers[HeaderAgentBodySHA256]
			transcript := append(agentSigPayload(tc.signMethod, tc.signPath, ts, digest), []byte("\naudience:"+tc.audience)...)
			headers[HeaderAgentSig] = hex.EncodeToString(ed25519.Sign(keys["scheduler"], transcript))
			req := httptest.NewRequest(http.MethodPost, "/agents/router/inbox", strings.NewReader(tc.sentBody))
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			req = mux.SetURLVars(req, map[string]string{"id": "router"})
			w := httptest.NewRecorder()
			h.HandleDeliver(w, req)
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, tc.response, w.Body.String())
			depth, _, _, err := store.Stats("router")
			require.NoError(t, err)
			require.Zero(t, depth)
		})
	}
}

func TestServiceSenderNamespacesAreIndependent(t *testing.T) {
	t.Parallel()
	h, store, keys := strictServiceHandler(t)
	for _, sender := range []string{"scheduler", "backup"} {
		body := `{"sender":"` + sender + `","payload":{},"idempotency_key":"shared"}`
		got := strictServicePost(t, h, keys[sender], sender, body, "relay-a")
		require.Equal(t, http.StatusCreated, got.Code, got.Body.String())
	}
	depth, _, _, err := store.Stats("router")
	require.NoError(t, err)
	require.Equal(t, 2, depth)
}

func TestAuthorityStorePreservesShippedStoreCapabilities(t *testing.T) {
	t.Parallel()
	base := NewMemoryStore()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, base.Register(&Agent{ID: "router", PublicKey: HexKey(pub)}))
	policy, err := ParseServiceDeliveryAllowlist("scheduler>router")
	require.NoError(t, err)
	secured := NewAuthorityStore(base, policy, "relay-a")

	require.Implements(t, (*updater)(nil), secured)
	require.Implements(t, (*ListErrorReporter)(nil), secured)
	require.Implements(t, (*PurgeReporter)(nil), secured)
	require.Implements(t, (*DeadLetterStore)(nil), secured)
	require.Implements(t, (*Transferrer)(nil), secured)
	require.Implements(t, (*Toucher)(nil), secured)
	require.Implements(t, (*DepthReporter)(nil), secured)

	agent, err := secured.Get("router")
	require.NoError(t, err)
	agent.Status = StatusOffline
	require.NoError(t, secured.Update(agent))
	require.NoError(t, secured.ListError())

	dl := &DeadLetter{MessageID: "expired-1", AgentID: "router", Payload: json.RawMessage(`{}`), ExpiredAt: time.Now().UTC()}
	added, err := secured.AppendDeadLetter(dl)
	require.NoError(t, err)
	require.True(t, added)
	letters, err := secured.ListDeadLetters("router", 10)
	require.NoError(t, err)
	require.Len(t, letters, 1)
}

func TestValidateServiceIdentitiesRejectsMissingAndKeyless(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "router"}))
	policy, err := ParseServiceDeliveryAllowlist("scheduler>router")
	require.NoError(t, err)
	require.Error(t, ValidateServiceIdentities(store, policy))
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, store.Register(&Agent{ID: "scheduler", PublicKey: HexKey(pub)}))
	require.Error(t, ValidateServiceIdentities(store, policy))
}
