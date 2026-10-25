package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/permissions"
)

// The optional interface supported the pre-implementation HTTP negative control.
// Every policy test drives the HTTP routes, not just this configuration seam.
func setServicePolicy(t *testing.T, h *Handler, policy string) error {
	t.Helper()
	setter, ok := any(h).(interface{ SetServiceDeliveryAllowlist(string) error })
	if !ok {
		t.Log("service policy setter not implemented; exercise existing HTTP behavior as the negative control")
		return nil
	}
	return setter.SetServiceDeliveryAllowlist(policy)
}

func serviceHarness(t *testing.T) (*cr36Harness, map[string]ed25519.PrivateKey) {
	return serviceHarnessWithPolicy(t, "scheduler>router,router>scheduler")
}

func serviceHarnessWithPolicy(t *testing.T, policy string) (*cr36Harness, map[string]ed25519.PrivateKey) {
	t.Helper()
	store := NewMemoryStore()
	handler := NewHandler(store)
	// Startup-only configuration must happen BEFORE the HTTP serving goroutine,
	// especially when the caller is a separate Python process (no Go HB edge).
	require.NoError(t, setServicePolicy(t, handler, policy))
	route := mux.NewRouter()
	route.HandleFunc("/agents", handler.HandleRegister).Methods("POST")
	route.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	route.HandleFunc("/agents/{id}/inbox", handler.HandleRetrieve).Methods("GET")
	route.HandleFunc("/agents/{id}/inbox/ack", handler.HandleAck).Methods("POST")
	server := httptest.NewServer(route)
	t.Cleanup(server.Close)
	h := &cr36Harness{server: server, store: store, handler: handler, client: server.Client()}
	keys := map[string]ed25519.PrivateKey{}
	for _, id := range []string{"scheduler", "router", "outsider", "ordinary"} {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		keys[id] = key
		raw, err := json.Marshal(map[string]string{"id": id, "public_key": hex.EncodeToString(pub)})
		require.NoError(t, err)
		code, out := h.send(t, "POST", "/agents", string(raw), nil)
		require.Equal(t, http.StatusCreated, code, out)
	}
	return h, keys
}

func serviceHeaders(key ed25519.PrivateKey, sender, method, path, body string, at time.Time) map[string]string {
	ts := strconv.FormatInt(at.Unix(), 10)
	digest := sha256Hex([]byte(body))
	return map[string]string{
		HeaderAgentID: sender, HeaderAgentTS: ts, HeaderAgentBodySHA256: digest,
		HeaderAgentSig: hex.EncodeToString(ed25519.Sign(key, agentSigPayload(method, path, ts, digest))),
	}
}

func TestServiceDeliveryAllowedAndSelfOnlyReads(t *testing.T) {
	t.Parallel()
	h, keys := serviceHarness(t)
	h.handler.SetRequireAgentSig(true)
	for _, pair := range [][2]string{{"scheduler", "router"}, {"router", "scheduler"}} {
		sender, target := pair[0], pair[1]
		path := "/agents/" + target + "/inbox"
		body := fmt.Sprintf(`{"sender":%q,"payload":{"task":"dispatch"},"model":" model/x ","provider":" provider/y ","reply_to":"crier-correlation-123","request_id":"request-123","session_id":"session-123"}`, sender)
		headers := serviceHeaders(keys[sender], sender, "POST", path, body, time.Now())
		code, out := h.send(t, "POST", path, body, headers)
		require.Equal(t, 201, code, out)
		// A valid sender signature never grants cross-agent GET or ack access.
		code, out = h.signGet(t, sender, keys[sender], path, "")
		require.Equal(t, 403, code, out)
		code, out = h.signGet(t, target, keys[target], path, "")
		require.Equal(t, 200, code, out)
		var inbox cr36Inbox
		require.NoError(t, json.Unmarshal([]byte(out), &inbox))
		require.Len(t, inbox.Messages, 1)
		msg := inbox.Messages[0]
		for k, v := range map[string]string{"sender": sender, "model": " model/x ", "provider": " provider/y ", "reply_to": "crier-correlation-123"} {
			require.JSONEq(t, strconv.Quote(v), string(msg[k]), k)
		}
		var msgID string
		require.NoError(t, json.Unmarshal(msg["id"], &msgID))
		ackPath := path + "/ack"
		ack := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, inbox.LeaseID, msgID)
		code, out = h.send(t, "POST", ackPath, ack, serviceHeaders(keys[sender], sender, "POST", ackPath, ack, time.Now()))
		require.Equal(t, 403, code, out)
		code, out = h.send(t, "POST", ackPath, ack, serviceHeaders(keys[target], target, "POST", ackPath, ack, time.Now()))
		require.Equal(t, 204, code, out)
		q, l, _, err := h.store.Stats(target)
		require.NoError(t, err)
		require.Zero(t, q+l)
	}
}

func TestServiceDeliveryRejectsBeforeEffects(t *testing.T) {
	t.Parallel()
	cases := []string{"missing", "missing digest", "empty digest", "wrong digest", "legacy signature", "uppercase digest", "tampered body", "invalid signature", "forged key", "keyless", "stale", "future", "invalid timestamp", "sender mismatch", "missing sender", "unregistered", "unlisted", "principal", "as_agent", "principal only", "reply authority", "tampered reply", "wrong path", "wrong method"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			h, keys := serviceHarness(t)
			h.handler.SetGlobalRateLimit(1)
			// Acquire prunes expired receipts, even if the attempted delivery
			// later abandons its own claim. Keep a canary to detect ANY call.
			replay := h.handler.idempotency
			replay.records["expired-canary"] = &idempotentReceipt{ID: "old", RecordedAt: time.Now().Add(-48 * time.Hour)}
			path := "/agents/router/inbox"
			body := `{"sender":"scheduler","payload":{"task":"dispatch"},"idempotency_key":"same-key"}`
			signer := "scheduler"
			switch name {
			case "sender mismatch":
				body = strings.Replace(body, `"scheduler"`, `"outsider"`, 1)
			case "keyless":
				a, err := h.store.Get("scheduler")
				require.NoError(t, err)
				a.PublicKey = nil
				require.NoError(t, h.store.(updater).Update(a))
			case "missing sender":
				body = strings.Replace(body, `"sender":"scheduler",`, ``, 1)
			case "unlisted", "reply authority":
				signer = "outsider"
				body = strings.Replace(body, `"scheduler"`, `"outsider"`, 1)
			case "unregistered":
				signer = "ghost"
				keys[signer] = keys["scheduler"]
				body = strings.Replace(body, `"scheduler"`, `"ghost"`, 1)
			case "principal":
				body = strings.TrimSuffix(body, "}") + `,"principal_id":"scheduler"}`
			case "as_agent":
				body = strings.TrimSuffix(body, "}") + `,"as_agent":"scheduler"}`
			case "principal only":
				body = `{"principal_id":"scheduler","as_agent":"scheduler","payload":{},"idempotency_key":"same-key"}`
			}
			if name == "reply authority" {
				body = strings.TrimSuffix(body, "}") + `,"reply_to":"scheduler"}`
			}
			headers := serviceHeaders(keys[signer], signer, "POST", path, body, time.Now())
			switch name {
			case "missing", "principal only":
				headers = nil
			case "missing digest":
				delete(headers, HeaderAgentBodySHA256)
			case "empty digest":
				headers[HeaderAgentBodySHA256] = ""
			case "legacy signature":
				ts := headers[HeaderAgentTS]
				headers[HeaderAgentSig] = hex.EncodeToString(ed25519.Sign(keys[signer], agentSigPayload("POST", path, ts, "")))
			case "uppercase digest":
				headers[HeaderAgentBodySHA256] = strings.ToUpper(headers[HeaderAgentBodySHA256])
			case "tampered body":
				body += " "
			case "wrong digest":
				headers[HeaderAgentBodySHA256] = strings.Repeat("0", 64)
			case "invalid signature":
				headers[HeaderAgentSig] = "bad"
			case "forged key":
				headers = serviceHeaders(keys["outsider"], "scheduler", "POST", path, body, time.Now())
			case "stale":
				headers = serviceHeaders(keys[signer], signer, "POST", path, body, time.Now().Add(-31*time.Second))
			case "future":
				headers = serviceHeaders(keys[signer], signer, "POST", path, body, time.Now().Add(32*time.Second))
			case "invalid timestamp":
				headers[HeaderAgentTS] = "yesterday"
			case "tampered reply":
				body = strings.TrimSuffix(body, "}") + `,"reply_to":"forged-correlation"}`
			case "wrong path":
				headers = serviceHeaders(keys[signer], signer, "POST", "/agents/scheduler/inbox", body, time.Now())
			case "wrong method":
				headers = serviceHeaders(keys[signer], signer, "GET", path, body, time.Now())
			}
			code, out := h.send(t, "POST", path, body, headers)
			require.Contains(t, []int{401, 403}, code, out)
			for _, id := range []string{"router", "scheduler", "outsider"} {
				q, l, _, err := h.store.Stats(id)
				require.NoError(t, err)
				require.Zero(t, q+l, id)
			}
			replay.mu.Lock()
			require.Contains(t, replay.records, "expired-canary")
			require.Len(t, replay.records, 1)
			require.Empty(t, replay.claims)
			replay.mu.Unlock()
			// Restore the keyless registration for the valid retry control.
			if name == "keyless" {
				a, err := h.store.Get("scheduler")
				require.NoError(t, err)
				a.PublicKey = HexKey(keys["scheduler"].Public().(ed25519.PublicKey))
				require.NoError(t, h.store.(updater).Update(a))
			}
			// Rejection must not even REGISTER an idempotency attempt, not just abandon it.
			_, outcome, attempt := h.handler.idempotency.Acquire("router", "same-key", time.Now())
			require.NotNil(t, attempt)
			require.NotEqual(t, idempotencyReplay, outcome)
			require.NotEqual(t, idempotencyBusy, outcome)
			attempt.Abandon()
			// Same key, corrected signature: not replayed and the sole rate token remains.
			valid := `{"sender":"scheduler","payload":{},"idempotency_key":"same-key"}`
			code, out = h.send(t, "POST", path, valid, serviceHeaders(keys["scheduler"], "scheduler", "POST", path, valid, time.Now()))
			require.Equal(t, 201, code, out)
			require.NotContains(t, out, `"idempotent_replay":true`)
			q, l, _, err := h.store.Stats("router")
			require.NoError(t, err)
			require.Equal(t, 1, q+l)
			// An unsigned replay must not retrieve the authenticated accept receipt.
			code, out = h.send(t, "POST", path, valid, nil)
			require.Equal(t, 401, code, out)
		})
	}
}

func TestServiceDeliveryOrdinaryCompatibility(t *testing.T) {
	t.Parallel()
	h, _ := serviceHarness(t)
	code, out := h.send(t, "POST", "/agents/ordinary/inbox", `{"principal_id":"human","as_agent":"personal","sender":"legacy","payload":{}}`, nil)
	require.Equal(t, 201, code, out)
	require.NoError(t, setServicePolicy(t, h.handler, ""))
	code, out = h.send(t, "POST", "/agents/router/inbox", `{"sender":"legacy","payload":{}}`, nil)
	require.Equal(t, 201, code, out)
}

func TestServiceDeliveryCapabilityCannotBypass(t *testing.T) {
	t.Parallel()
	h, _ := serviceHarness(t)
	a, err := h.store.Get("router")
	require.NoError(t, err)
	a.Capabilities = []string{"service"}
	require.NoError(t, h.store.(updater).Update(a))
	h.handler.SetGlobalRateLimit(1)
	route := mux.NewRouter()
	route.HandleFunc("/capabilities/{capability}/inbox", h.handler.HandleDeliverByCapability).Methods("POST")
	req := httptest.NewRequest("POST", "/capabilities/service/inbox", strings.NewReader(`{"sender":"scheduler","payload":{},"idempotency_key":"cap-key"}`))
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, 403, w.Code, w.Body.String())
	require.Empty(t, h.handler.capCursors)
	q, l, _, err := h.store.Stats("router")
	require.NoError(t, err)
	require.Zero(t, q+l)
	code, out := h.send(t, "POST", "/agents/ordinary/inbox", `{"payload":{}}`, nil)
	require.Equal(t, 201, code, out)
}

func TestServiceDeliveryPolicyValidation(t *testing.T) {
	t.Parallel()
	h := NewHandler(NewMemoryStore())
	for _, raw := range []string{"* >router", "scheduler>*", "scheduler", "scheduler>router>other", ">router", "scheduler>", "scheduler>router,", "scheduler>router,,router>scheduler", "scheduler name>router", "scheduler>/router"} {
		require.Error(t, setServicePolicy(t, h, raw), raw)
	}
	require.NoError(t, setServicePolicy(t, h, " scheduler>router , router>scheduler "))
}

// A human ACL remains authoritative for ordinary recipients. A verified service
// pair is a distinct authority and needs no fabricated human principal/binding.
func TestServiceDeliveryIndependentOfHumanACL(t *testing.T) {
	t.Parallel()
	h, keys := serviceHarness(t)
	ps, err := permissions.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	for _, id := range []string{"router", "ordinary"} {
		require.NoError(t, ps.Append(context.Background(), (&permissions.AgentInfo{ID: id, Class: permissions.ClassService}).Record(time.Now())))
	}
	h.handler.SetPermissionsChecker(permissions.NewChecker(ps))
	body := `{"sender":"scheduler","payload":{}}`
	path := "/agents/router/inbox"
	code, out := h.send(t, "POST", path, body, serviceHeaders(keys["scheduler"], "scheduler", "POST", path, body, time.Now()))
	require.Equal(t, 201, code, out)
	code, out = h.send(t, "POST", "/agents/ordinary/inbox", body, nil)
	require.Equal(t, 403, code, out)
	q, l, _, err := h.store.Stats("ordinary")
	require.NoError(t, err)
	require.Zero(t, q+l)
}

func TestServiceDeliveryHintsKeepDocumentedForms(t *testing.T) {
	t.Parallel()
	spellings := append(append([]hintSpelling{}, cr36DefaultSpellings...), hintSpelling{name: "explicit", raw: `" Model/Provider-X "`, want: `" Model/Provider-X "`})
	for _, model := range spellings {
		for _, provider := range spellings {
			t.Run(model.name+"/"+provider.name, func(t *testing.T) {
				h, keys := serviceHarness(t)
				body := strings.TrimSuffix(dispatchBody(model.raw, provider.raw, "correlation-raw"), "}") + `,"sender":"scheduler"}`
				path := "/agents/router/inbox"
				code, out := h.send(t, "POST", path, body, serviceHeaders(keys["scheduler"], "scheduler", "POST", path, body, time.Now()))
				require.Equal(t, 201, code, out)
				inbox := h.poll(t, "router", 10)
				require.Len(t, inbox.Messages, 1)
				for key, want := range map[string]string{"model": model.want, "provider": provider.want, "reply_to": `"correlation-raw"`} {
					raw, present := cr36Message(inbox.Messages[0], key)
					require.Equal(t, want != "", present, key)
					require.Equal(t, want, raw, key)
				}
			})
		}
	}
}

func TestServiceDeliveryUnconfiguredHumanACLStillWorks(t *testing.T) {
	t.Parallel()
	h := newCR36ACL(t)
	require.NoError(t, setServicePolicy(t, h.handler, "scheduler>router,router>scheduler"))
	// These legacy recipients are classed and have real human bindings/grants,
	// but are NOT the configured service recipient IDs above.
	code, out := h.sendAs(t, cr36Router, "prin_sched", cr36Scheduler, `{"payload":{}}`, nil)
	require.Equal(t, 201, code, out)
	code, out = h.sendAs(t, cr36Router, "prin_outsider", cr36Outsider, `{"payload":{}}`, nil)
	require.Equal(t, 403, code, out)
}

func TestServiceDeliveryPolicyIsDirectedAndCaseSensitive(t *testing.T) {
	t.Parallel()
	h, keys := serviceHarness(t)
	require.NoError(t, setServicePolicy(t, h.handler, "scheduler>router,outsider>scheduler"))
	body := `{"sender":"router","reply_to":"scheduler","payload":{}}`
	path := "/agents/scheduler/inbox"
	code, out := h.send(t, "POST", path, body, serviceHeaders(keys["router"], "router", "POST", path, body, time.Now()))
	require.Equal(t, 403, code, out)
	require.NoError(t, setServicePolicy(t, h.handler, "Scheduler>router"))
	body = `{"sender":"scheduler","payload":{}}`
	path = "/agents/router/inbox"
	code, out = h.send(t, "POST", path, body, serviceHeaders(keys["scheduler"], "scheduler", "POST", path, body, time.Now()))
	require.Equal(t, 403, code, out)
	// A malformed reconfiguration must preserve the previous protected recipient.
	require.Error(t, setServicePolicy(t, h.handler, "scheduler>*"))
	code, out = h.send(t, "POST", path, body, nil)
	require.Equal(t, 401, code, out)
}
