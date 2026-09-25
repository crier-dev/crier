package relay

// CR-FEAT-029 (realms) — relay lane.
//
// The relay is where the "one noisy neighbour, one shared fate" failure was
// most concrete, so this file measures the two properties the acceptance names:
//
//   - a message cannot cross namespaces implicitly: two realms subscribed to
//     the IDENTICAL topic name are disjoint sets, and a publish in one reaches
//     only its own — asserted through the real HTTP handler, not by calling
//     PublishIn directly;
//   - two namespaces with different rate limits do not interfere: the tight
//     realm's cap is exhausted by a flood while the quiet realm's publisher
//     keeps being accepted, and the same agent id in two realms keeps two
//     independent budgets.
//
// It also pins the single-namespace side: with no namespaces wired, the
// pre-CR-FEAT-029 API (Publish/Subscribe/CheckRateLimit) behaves exactly as it
// did and /relay/topics gains no field.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
)

// nsRegistry builds a registry from a namespace document, failing the test on a
// policy the server itself would refuse at boot.
func nsRegistry(t *testing.T, raw string) *namespace.Registry {
	t.Helper()
	reg, err := namespace.Parse(raw, nil)
	if err != nil {
		t.Fatalf("namespace.Parse: %v", err)
	}
	return reg
}

// publishAs posts one publish as agentID in nsName (empty = no namespace header).
func publishAs(t *testing.T, h http.Handler, nsName, agentID, topic, event string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"topic":"` + topic + `","event":` + event + `}`
	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(body))
	req.Header.Set("X-Agent-ID", agentID)
	if nsName != "" {
		req.Header.Set(namespace.HeaderNamespace, nsName)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// recvFrame returns the next frame on a subscription, or nil if none arrives
// within the grace window. A short window is enough: delivery on the same
// goroutine is immediate, and a cross-realm leak would arrive just as fast.
func recvFrame(ch <-chan []byte) []byte {
	select {
	case frame := <-ch:
		return frame
	case <-time.After(150 * time.Millisecond):
		return nil
	}
}

// TestRelayNamespacesAreDisjoint is the isolation acceptance: the SAME topic
// name in two realms must not be a shared channel.
func TestRelayNamespacesAreDisjoint(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)

	acmeCh, acmeUnsub := r.SubscribeIn("acme", "orders.created")
	defer acmeUnsub()
	globexCh, globexUnsub := r.SubscribeIn("globex", "orders.created")
	defer globexUnsub()

	if err := r.PublishIn("acme", "orders.created", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatalf("publish into acme: %v", err)
	}
	if frame := recvFrame(acmeCh); frame == nil {
		t.Fatal("the publishing realm's subscriber received nothing")
	} else if !strings.Contains(string(frame), `"topic":"orders.created"`) {
		t.Errorf("frame = %s, want the literal topic", frame)
	}
	if frame := recvFrame(globexCh); frame != nil {
		t.Fatalf("a message crossed namespaces: globex received %s", frame)
	}

	// And the other direction, so a one-way bug cannot pass this test.
	if err := r.PublishIn("globex", "orders.created", json.RawMessage(`{"n":2}`)); err != nil {
		t.Fatalf("publish into globex: %v", err)
	}
	if frame := recvFrame(globexCh); frame == nil {
		t.Fatal("globex's own publish was not delivered")
	}
	if frame := recvFrame(acmeCh); frame != nil {
		t.Fatalf("a message crossed namespaces backwards: acme received %s", frame)
	}
}

// TestRelayWildcardIsNamespaceScoped: a wildcard subscription is scoped to its
// realm too — a pattern in one realm must not match the other's topics, which
// is the shape a realm-blind wildcard would leak through.
func TestRelayWildcardIsNamespaceScoped(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)

	acmeCh, unsub := r.SubscribeIn("acme", "orders.>")
	defer unsub()

	if err := r.PublishIn("globex", "orders.created", json.RawMessage(`{"realm":"globex"}`)); err != nil {
		t.Fatalf("publish into globex: %v", err)
	}
	if frame := recvFrame(acmeCh); frame != nil {
		t.Fatalf("a wildcard subscriber received another realm's message: %s", frame)
	}
	if err := r.PublishIn("acme", "orders.created", json.RawMessage(`{"realm":"acme"}`)); err != nil {
		t.Fatalf("publish into acme: %v", err)
	}
	if frame := recvFrame(acmeCh); frame == nil {
		t.Fatal("the wildcard subscriber did not receive its own realm's message")
	}
}

// TestRelayDefaultNamespaceIsItsOwnRealm: a deployment WITH namespaces must not
// let an unqualified publish (the default realm) reach a named realm's
// subscribers, nor the reverse. The default realm is not a wildcard.
func TestRelayDefaultNamespaceIsItsOwnRealm(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)

	namedCh, unsubNamed := r.SubscribeIn("acme", "topic.a")
	defer unsubNamed()
	defaultCh, unsubDefault := r.Subscribe("topic.a")
	defer unsubDefault()

	publishAs(t, publishServer(r), "", "agent-a", "topic.a", `{"from":"default"}`)
	if frame := recvFrame(defaultCh); frame == nil {
		t.Fatal("the default realm's subscriber received nothing")
	}
	if frame := recvFrame(namedCh); frame != nil {
		t.Fatalf("an unqualified publish reached a named realm: %s", frame)
	}

	publishAs(t, publishServer(r), "acme", "agent-a", "topic.a", `{"from":"acme"}`)
	if frame := recvFrame(namedCh); frame == nil {
		t.Fatal("the named realm's subscriber received nothing")
	}
	if frame := recvFrame(defaultCh); frame != nil {
		t.Fatalf("a named realm's publish reached the default realm: %s", frame)
	}
}

// TestRelayPerNamespaceRateLimitsDoNotInterfere is the noisy-neighbour
// acceptance at the relay: the tight realm's cap must not touch the quiet
// realm's publishers, and the same agent id must keep two separate budgets.
func TestRelayPerNamespaceRateLimitsDoNotInterfere(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[
		{"name":"tight","rate_limit_per_minute":3},
		{"name":"loose","rate_limit_per_minute":25}
	]}`)
	r := New(100) // the deployment-wide cap the namespaces override
	r.SetNamespacePolicies(reg)
	h := publishServer(r)

	// A flood in the tight realm: the 4th publish is refused.
	for i := 0; i < 3; i++ {
		if rec := publishAs(t, h, "tight", "noisy", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
			t.Fatalf("tight publish %d = %d, want 202 (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := publishAs(t, h, "tight", "noisy", "t", `{"i":1}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the tight realm's 4th publish = %d, want 429", rec.Code)
	}

	// The quiet realm is untouched: neither the other agent nor the SAME agent
	// id publishing there has spent any budget.
	for i := 0; i < 10; i++ {
		if rec := publishAs(t, h, "loose", "quiet", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
			t.Fatalf("loose publish %d = %d, want 202 (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := publishAs(t, h, "loose", "noisy", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("the flooding agent's own id must have a separate budget in another realm: got %d", rec.Code)
	}
	// And the default realm is its own budget again (deployment cap 100).
	if rec := publishAs(t, h, "", "noisy", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("default-realm publish = %d, want 202", rec.Code)
	}
}

// TestRelayNamespaceCapOfZeroDisablesLimiting: 0 is a declared policy value
// ("this realm is not rate limited"), not a typo, and it is realm-local.
func TestRelayNamespaceCapOfZeroDisablesLimiting(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"open","rate_limit_per_minute":0},{"name":"closed","rate_limit_per_minute":1}]}`)
	r := New(100)
	r.SetNamespacePolicies(reg)
	h := publishServer(r)

	for i := 0; i < 12; i++ {
		if rec := publishAs(t, h, "open", "flooder", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
			t.Fatalf("unlimited realm publish %d = %d, want 202", i+1, rec.Code)
		}
	}
	if rec := publishAs(t, h, "closed", "agent", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("first publish in a 1/min realm = %d, want 202", rec.Code)
	}
	if rec := publishAs(t, h, "closed", "agent", "t", `{"i":1}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second publish in a 1/min realm = %d, want 429", rec.Code)
	}
}

// TestRelayUnknownNamespaceIsRefused: a name the server does not serve is a
// 400 naming the problem. It is never mapped back to the default realm, which
// would route a caller's events somewhere it did not name.
func TestRelayUnknownNamespaceIsRefused(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)
	h := publishServer(r)

	rec := publishAs(t, h, "ghost", "agent", "t", `{"i":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("publish into an undeclared namespace = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UNKNOWN_NAMESPACE") {
		t.Errorf("body = %s, want a named UNKNOWN_NAMESPACE refusal", rec.Body.String())
	}

	// The subscribe lane answers the same way, and refuses BEFORE the upgrade
	// so a rejected subscription never holds a socket.
	req := httptest.NewRequest(http.MethodGet, "/relay/subscribe/t?namespace=ghost", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("subscribe into an undeclared namespace = %d, want 400", rec.Code)
	}
}

// TestRelayNamespaceTokenPosture: a realm that declares the token posture
// requires its own credential, on both lanes, and the shared posture ignores it.
func TestRelayNamespaceTokenPosture(t *testing.T) {
	t.Setenv("RELAY_TOKEN", "s3cret")
	reg := nsRegistry(t, `{"namespaces":[
		{"name":"private","auth":"token","token_ref":"env:RELAY_TOKEN"},
		{"name":"public"}
	]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)
	h := publishServer(r)

	// No credential: refused, and nothing is published.
	privateCh, unsub := r.SubscribeIn("private", "t")
	defer unsub()
	rec := publishAs(t, h, "private", "agent", "t", `{"i":1}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("publish into a token realm without its token = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if frame := recvFrame(privateCh); frame != nil {
		t.Fatal("a refused publish was delivered anyway")
	}

	// With the credential: accepted.
	req := httptest.NewRequest(http.MethodPost, "/relay/publish", strings.NewReader(`{"topic":"t","event":{"i":1}}`))
	req.Header.Set("X-Agent-ID", "agent")
	req.Header.Set(namespace.HeaderNamespace, "private")
	req.Header.Set(namespace.HeaderNamespaceToken, "s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("publish with the realm credential = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if frame := recvFrame(privateCh); frame == nil {
		t.Fatal("an authorized publish was not delivered")
	}

	// The shared-posture realm must not start demanding a token.
	if rec := publishAs(t, h, "public", "agent", "t", `{"i":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("a shared-posture realm must accept a plain publish: got %d", rec.Code)
	}
}

// TestRelayNamespaceInBodyIsRefused: the realm must be known before the body is
// read (so the per-realm rate limit is exact), which makes a body member a
// value the server does not honor — and an unhonored value is an error, never
// a no-op (DF-CRIER-180).
func TestRelayNamespaceInBodyIsRefused(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)
	h := publishServer(r)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/relay/publish",
		strings.NewReader(`{"topic":"t","event":{"i":1},"namespace":"acme"}`))
	req.Header.Set("X-Agent-ID", "agent")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body namespace = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "X-Crier-Namespace header") {
		t.Errorf("body = %s, want it to point at the header", rec.Body.String())
	}
}

// TestRelayDisagreeingNamespaceSpellingsAreRefused: the header and the query
// parameter must agree; the realm is never picked by precedence between two
// disagreeing sources.
func TestRelayDisagreeingNamespaceSpellingsAreRefused(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)
	h := publishServer(r)

	req := httptest.NewRequest(http.MethodGet, "/relay/subscribe/t?namespace=globex", nil)
	req.Header.Set(namespace.HeaderNamespace, "acme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disagreeing realm spellings = %d, want 400", rec.Code)
	}
}

// TestRelayTopicsCarryTheNamespace: the inventory is where an operator sees
// which realms are actually in use — the same literal topic name appearing once
// per realm is the isolation made visible.
func TestRelayTopicsCarryTheNamespace(t *testing.T) {
	reg := nsRegistry(t, `{"namespaces":[{"name":"acme"}]}`)
	r := New(0)
	r.SetNamespacePolicies(reg)

	_, unsubA := r.SubscribeIn("acme", "shared.topic")
	defer unsubA()
	_, unsubD := r.Subscribe("shared.topic")
	defer unsubD()

	byNamespace := map[string]TopicInfo{}
	for _, info := range r.Topics() {
		if info.Name != "shared.topic" {
			continue
		}
		byNamespace[info.Namespace] = info
	}
	if len(byNamespace) != 2 {
		t.Fatalf("topics = %+v, want the same name once per realm", r.Topics())
	}
	if _, ok := byNamespace["acme"]; !ok {
		t.Error("the named realm's topic must be reported with its namespace")
	}
	if _, ok := byNamespace[""]; !ok {
		t.Error("the default realm's topic must be reported with an empty namespace")
	}
}

// TestSingleNamespaceRelayIsUnchanged pins the other half of the acceptance:
// with no namespaces configured, the pre-CR-FEAT-029 API is the whole contract
// and the topic inventory is byte-identical (no `namespace` member).
func TestSingleNamespaceRelayIsUnchanged(t *testing.T) {
	r := New(100) // deliberately NOT wired with a namespace registry.

	ch, unsub := r.Subscribe("plain.topic")
	defer unsub()
	if err := r.Publish("plain.topic", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	frame := recvFrame(ch)
	if frame == nil {
		t.Fatal("the pre-CR-FEAT-029 publish/subscribe path stopped working")
	}
	if want := `{"topic":"plain.topic","event":{"ok":true}}`; string(frame) != want {
		t.Errorf("frame = %s, want %s", frame, want)
	}
	if !r.CheckRateLimit("agent-a") {
		t.Error("the single-namespace rate limit must allow the first publish")
	}

	topics, err := json.Marshal(r.Topics())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(topics), "namespace") {
		t.Errorf("a single-namespace deployment's /relay/topics must not grow a namespace member: %s", topics)
	}
}
