package registry

// CR-FEAT-026 — capability-routed delivery, unit-level, exercised through a real
// mux router and the real handlers (in-memory store, signature enforcement off,
// which is how most unit tests in this package drive the agent-scoped routes).
//
// The acceptance criteria this file drives: a deliver-to-capability LANDS on a
// holder and is retrievable and ackable; the selection really rotates over the
// live holders and really skips a dead one; ZERO holders is the named
// NO_CAPABLE_AGENT refusal with nothing stored anywhere; and deliver-by-id is
// untouched — byte-identical accept body, same idempotency namespace.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/webhook"
)

// capabilityProbe is the capability the tests dial.
const capabilityProbe = "solver"

// capabilityHolder registers an agent advertising caps and returns its id body.
func capabilityHolder(t *testing.T, store Store, id string, caps ...string) {
	t.Helper()
	_, pub := newTestPubKey(t)
	if err := store.Register(&Agent{ID: id, PublicKey: HexKey(pub), Capabilities: caps}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// deliverToCapability posts body to POST /capabilities/{capability}/inbox and
// returns the recorder plus the decoded body.
func deliverToCapability(t *testing.T, router http.Handler, capability, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, "/capabilities/"+capability+"/inbox", body)
	return rec, decodeJSONBody(t, rec)
}

// deliverToAgent posts body to POST /agents/{id}/inbox.
func deliverToAgent(t *testing.T, router http.Handler, agentID, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, "/agents/"+agentID+"/inbox", body)
	return rec, decodeJSONBody(t, rec)
}

func doRequest(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

// inboxRead is the GET /agents/{id}/inbox response, narrowed to what these tests
// assert: the leased ids, the payload (base64 on the wire — Go []byte JSON
// encoding), the lease to ack with, and the queue depth.
type inboxRead struct {
	Messages []struct {
		ID      string `json:"id"`
		Payload string `json:"payload"`
	} `json:"messages"`
	LeaseID     string `json:"lease_id"`
	QueueDepth  int    `json:"queue_depth"`
	LeasedCount int    `json:"leased_count"`
}

// readInbox performs a signed-less retrieve (signature enforcement is off) and
// returns the decoded read. leaseQuery, when set, is the query string appended
// to the read (e.g. "lease=1") so a test can take a SHORT lease on purpose.
func readInbox(t *testing.T, router http.Handler, agentID string, leaseQuery ...string) inboxRead {
	t.Helper()
	path := "/agents/" + agentID + "/inbox"
	if len(leaseQuery) > 0 && leaseQuery[0] != "" {
		path += "?" + leaseQuery[0]
	}
	rec := doRequest(t, router, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var out inboxRead
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode retrieve for %s: %v (%s)", agentID, err, rec.Body.String())
	}
	return out
}

// ackInbox acks the given ids under lease and asserts the documented 204.
func ackInbox(t *testing.T, router http.Handler, agentID, leaseID string, ids []string) {
	t.Helper()
	body, _ := json.Marshal(ackRequest{LeaseID: leaseID, MessageIDs: ids})
	rec := doRequest(t, router, http.MethodPost, "/agents/"+agentID+"/inbox/ack", string(body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ack %v for %s = %d: %s", ids, agentID, rec.Code, rec.Body.String())
	}
}

// inboxStats reads queue_depth / leased_count through the route.
func inboxStats(t *testing.T, router http.Handler, agentID string) (queueDepth, leasedCount int) {
	t.Helper()
	rec := doRequest(t, router, http.MethodGet, "/agents/"+agentID+"/inbox/stats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats %s = %d: %s", agentID, rec.Code, rec.Body.String())
	}
	var out struct {
		QueueDepth  int `json:"queue_depth"`
		LeasedCount int `json:"leased_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode stats for %s: %v", agentID, err)
	}
	return out.QueueDepth, out.LeasedCount
}

// ageRow backdates a row's liveness evidence, which is how a holder that stopped
// heartbeating looks a window later (CR-FEAT-024). The store's own Touch only
// ADVANCES last_seen, so a test needs this seam; it holds the store's own lock.
func ageRow(t *testing.T, store Store, id string, age time.Duration) {
	t.Helper()
	ms, ok := store.(*MemoryStore)
	if !ok {
		t.Fatalf("ageRow needs the in-memory store, got %T", store)
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	row, ok := ms.agents[id]
	if !ok {
		t.Fatalf("ageRow: no row for %s", id)
	}
	row.LastSeen = time.Now().Add(-age)
}

// ---------------------------------------------------------------------------
// AC 1 — deliver to a capability lands on a holder and is retrievable/ackable
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryLandsOnAHolderAndIsAckable(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	capabilityHolder(t, store, "worker-a", capabilityProbe, "relay")
	capabilityHolder(t, store, "worker-b", capabilityProbe)

	rec, body := deliverToCapability(t, router, capabilityProbe,
		`{"payload":{"job":"build-1"},"sender":"alice"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver-to-capability = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if body["transport"] != "inbox" {
		t.Errorf("transport = %v, want inbox", body["transport"])
	}
	if body["capability"] != capabilityProbe {
		t.Errorf("accept does not name the capability dialed: %v", body["capability"])
	}
	target, _ := body["target"].(string)
	if target != "worker-a" && target != "worker-b" {
		t.Fatalf("accept target = %q, want one of the holders (the sender must learn which worker took it)", target)
	}

	// The message is in THAT holder's durable inbox — and NOT fanned out to the
	// other holder: the selector picks one worker, it does not broadcast.
	read := readInbox(t, router, target)
	if len(read.Messages) != 1 {
		t.Fatalf("%s inbox holds %d messages, want 1", target, len(read.Messages))
	}
	other := "worker-a"
	if target == "worker-a" {
		other = "worker-b"
	}
	if otherRead := readInbox(t, router, other); len(otherRead.Messages) != 0 {
		t.Errorf("capability delivery reached %d message(s) in %s too — the pool must not be fanned out", len(otherRead.Messages), other)
	}

	// The payload travelled intact (base64 on the wire).
	raw, err := base64.StdEncoding.DecodeString(read.Messages[0].Payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v (%q)", err, read.Messages[0].Payload)
	}
	var got, want map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	_ = json.Unmarshal([]byte(`{"job":"build-1"}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %v, want %v", got, want)
	}

	// …and it is ACKABLE through the ordinary ack route, releasing the lease.
	if read.LeaseID == "" {
		t.Fatal("a non-empty claim must come with a lease id")
	}
	ackInbox(t, router, target, read.LeaseID, []string{read.Messages[0].ID})
	after := readInbox(t, router, target)
	if len(after.Messages) != 0 || after.QueueDepth != 0 {
		t.Errorf("after ack: messages=%d queue_depth=%d, want 0/0", len(after.Messages), after.QueueDepth)
	}
}

// ---------------------------------------------------------------------------
// AC — the selection rule: round-robin over the live holders
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryRoundRobinsOverLiveHolders(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	for _, id := range []string{"worker-a", "worker-b", "worker-c"} {
		capabilityHolder(t, store, id, capabilityProbe)
	}

	// Six deliveries over a three-holder pool: the rotation is id-ascending and
	// therefore exact — a,b,c,a,b,c — not merely balanced (the pool order is
	// established by the selector, never inherited from map iteration order).
	want := []string{"worker-a", "worker-b", "worker-c", "worker-a", "worker-b", "worker-c"}
	for i, wantTarget := range want {
		rec, body := deliverToCapability(t, router, capabilityProbe,
			fmt.Sprintf(`{"payload":{"n":%d},"sender":"alice"}`, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("delivery %d = %d: %s", i, rec.Code, rec.Body.String())
		}
		if body["target"] != wantTarget {
			t.Fatalf("delivery %d went to %v, want %s (round-robin, id-ascending pool)", i, body["target"], wantTarget)
		}
	}

	// Each holder took exactly two of the six.
	for _, id := range []string{"worker-a", "worker-b", "worker-c"} {
		if depth, _ := inboxStats(t, router, id); depth != 2 {
			t.Errorf("%s queue_depth = %d, want 2 (fair rotation)", id, depth)
		}
	}

	// The cursor is PER CAPABILITY, not one global turn counter: a second
	// capability with a different pool rotates on its own.
	capabilityHolder(t, store, "solver-d", "translator")
	capabilityHolder(t, store, "solver-e", "translator")
	_, first := deliverToCapability(t, router, "translator", `{"payload":{"n":0}}`)
	if first["target"] != "solver-d" {
		t.Errorf("first translator delivery went to %v, want solver-d (a fresh cursor for a fresh capability)", first["target"])
	}
	// And the solver cursor continued where it left off: six deliveries in, the
	// seventh turn belongs to worker-a.
	if _, seventh := deliverToCapability(t, router, capabilityProbe, `{"payload":{"n":6}}`); seventh["target"] != "worker-a" {
		t.Errorf("seventh solver delivery went to %v, want worker-a (per-capability cursor)", seventh["target"])
	}
}

func TestCapabilityDeliverySkipsStaleHoldersAndFallsBackWhenNoneLive(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	capabilityHolder(t, store, "worker-live", capabilityProbe)
	capabilityHolder(t, store, "worker-dead", capabilityProbe)
	// Ten minutes is far outside the documented staleness window (90s): the row
	// reports `stale`, exactly as a crashed holder does (CR-FEAT-024).
	ageRow(t, store, "worker-dead", 10*time.Minute)

	for i := 0; i < 4; i++ {
		_, body := deliverToCapability(t, router, capabilityProbe, fmt.Sprintf(`{"payload":{"n":%d}}`, i))
		if body["target"] != "worker-live" {
			t.Fatalf("delivery %d went to %v — a stale holder must absorb no work while a live one exists", i, body["target"])
		}
	}
	if depth, _ := inboxStats(t, router, "worker-dead"); depth != 0 {
		t.Errorf("the stale holder received %d message(s)", depth)
	}

	// Nobody live is NOT the zero-holder refusal: the pool falls back to all
	// holders, because the inbox is durable and the message can wait for a
	// worker that comes back (and is reported by the expiry receipt if none
	// does). Ages both rows out of the window first.
	ageRow(t, store, "worker-live", 10*time.Minute)
	rec, body := deliverToCapability(t, router, capabilityProbe, `{"payload":{"n":9}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("delivery with no live holder = %d, want 201 (durable fallback, not a refusal): %s", rec.Code, rec.Body.String())
	}
	if target, _ := body["target"].(string); target != "worker-live" && target != "worker-dead" {
		t.Errorf("fallback target = %v, want one of the holders", body["target"])
	}
}

// ---------------------------------------------------------------------------
// AC — zero holders is a NAMED error, never a silent drop
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryWithZeroHoldersIsANamedRefusal(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	// A holder of a DIFFERENT capability, so the refusal cannot come from an
	// empty registry: it must come from the capability genuinely being unheld.
	capabilityHolder(t, store, "worker-relay", "relay")

	rec, body := deliverToCapability(t, router, capabilityProbe, `{"payload":{"job":"nobody-home"},"sender":"alice"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("zero-holder deliver = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if body["error"] != ErrNoCapableAgent {
		t.Errorf("error = %v, want the named %s", body["error"], ErrNoCapableAgent)
	}
	if body["capability"] != capabilityProbe {
		t.Errorf("refusal must name the capability dialed, got %v", body["capability"])
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "local") {
		t.Errorf("refusal detail should state the local scope of selection, got %q", detail)
	}

	// Nothing was stored anywhere (the whole point: never a silent drop).
	if depth, _ := inboxStats(t, router, "worker-relay"); depth != 0 {
		t.Errorf("the refusal stored %d message(s) in an unrelated inbox", depth)
	}
	// And it is NOT reported as the by-id agent-not-found: the two refusals say
	// different things, which is why the code is named.
	if _, byID := deliverToAgent(t, router, "worker-relay", `{"payload":{"job":"x"}}`); byID["error"] == ErrNoCapableAgent {
		t.Error("a by-id delivery must not be answered with the capability refusal")
	}
}

// ---------------------------------------------------------------------------
// AC — existing deliver-by-id is untouched
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryLeavesDeliverByIDUntouched(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	capabilityHolder(t, store, "worker-a", capabilityProbe)

	rec, _ := deliverToAgent(t, router, "worker-a", `{"payload":{"job":"build-1"},"sender":"alice"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver-by-id = %d: %s", rec.Code, rec.Body.String())
	}
	// The routing fields belong to the ORIGIN the sender chose: a by-id accept
	// must not grow a capability or a target it was never asked about.
	raw := rec.Body.String()
	if strings.Contains(raw, "capability") || strings.Contains(raw, `"target"`) {
		t.Errorf("by-id accept body gained the routing fields: %s", raw)
	}
	if !strings.Contains(raw, `"transport":"inbox"`) {
		t.Errorf("by-id accept body is not the documented shape: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// AC — a retry must not dispatch the same work to a second worker
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryIdempotencyIsScopedToTheCapability(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	capabilityHolder(t, store, "worker-a", capabilityProbe)
	capabilityHolder(t, store, "worker-b", capabilityProbe)

	body := `{"payload":{"job":"build-1"},"sender":"alice","idempotency_key":"job-1"}`
	rec, first := deliverToCapability(t, router, capabilityProbe, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first delivery = %d: %s", rec.Code, rec.Body.String())
	}
	rec2, replay := deliverToCapability(t, router, capabilityProbe, body)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("replay = %d, want the original 201: %s", rec2.Code, rec2.Body.String())
	}
	if replay["idempotent_replay"] != true {
		t.Fatalf("second delivery under the same key is not a replay: %s", rec2.Body.String())
	}
	if replay["id"] != first["id"] {
		t.Errorf("replay id = %v, want the original %v", replay["id"], first["id"])
	}
	if replay["target"] != first["target"] {
		t.Errorf("replay names %v, want the SAME holder as the original accept (%v)", replay["target"], first["target"])
	}
	if replay["capability"] != capabilityProbe {
		t.Errorf("replay lost the capability it was dialed under: %v", replay["capability"])
	}
	// Exactly one message exists in the pool: the retry dispatched nothing.
	total := 0
	for _, id := range []string{"worker-a", "worker-b"} {
		depth, _ := inboxStats(t, router, id)
		total += depth
	}
	if total != 1 {
		t.Errorf("pool holds %d messages after a keyed retry, want 1", total)
	}

	// The same key against a NAMED agent is a different delivery namespace —
	// scoping the capability key must not swallow a by-id delivery.
	_, byID := deliverToAgent(t, router, "worker-b", `{"payload":{"job":"build-1"},"sender":"alice","idempotency_key":"job-1"}`)
	if byID["idempotent_replay"] == true {
		t.Error("a by-id delivery was answered as a replay of the capability delivery's accept")
	}

	// A replay is answered from the receipt: it survives the pool emptying, and
	// it does not consume a turn in the rotation (the registry is not consulted).
	for _, id := range []string{"worker-a", "worker-b"} {
		if err := store.Unregister(id); err != nil {
			t.Fatalf("unregister %s: %v", id, err)
		}
	}
	rec3, afterEmpty := deliverToCapability(t, router, capabilityProbe, body)
	if rec3.Code != http.StatusCreated || afterEmpty["idempotent_replay"] != true {
		t.Fatalf("replay after the pool emptied = %d %s, want the recorded 201 replay", rec3.Code, rec3.Body.String())
	}
	if afterEmpty["target"] != first["target"] {
		t.Errorf("replay after the pool emptied names %v, want the original holder %v", afterEmpty["target"], first["target"])
	}
}

// ---------------------------------------------------------------------------
// AC — a holder that dies mid-lease: the existing lease/TTL path carries it
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryHolderDiesMidLeaseIsRequeued(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	capabilityHolder(t, store, "worker-a", capabilityProbe)

	rec, body := deliverToCapability(t, router, capabilityProbe, `{"payload":{"job":"survives-a-dead-worker"},"sender":"alice"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver = %d: %s", rec.Code, rec.Body.String())
	}
	target, _ := body["target"].(string)

	// The holder CLAIMS the message and then dies: a one-second lease, no ack.
	claimed := readInbox(t, router, target, "lease=1")
	if len(claimed.Messages) != 1 {
		t.Fatalf("claimed %d messages, want 1", len(claimed.Messages))
	}
	messageID := claimed.Messages[0].ID
	if depth, leased := inboxStats(t, router, target); depth != 1 || leased != 1 {
		t.Fatalf("while leased: queue_depth=%d leased_count=%d, want 1/1", depth, leased)
	}

	// The worker is GONE — no ack ever comes. The lease expires and the message
	// returns to the queue it was delivered to, so nothing is lost to the
	// capability route: this is the existing lease/ack rule, unchanged.
	time.Sleep(1200 * time.Millisecond)
	again := readInbox(t, router, target)
	if len(again.Messages) != 1 {
		t.Fatalf("after lease expiry the holder reclaimed %d messages, want the same 1 (requeue)", len(again.Messages))
	}
	if again.Messages[0].ID != messageID {
		t.Errorf("requeued id = %s, want the original %s", again.Messages[0].ID, messageID)
	}

	// And the TTL half is the existing one too: a capability-routed message that
	// expires unacknowledged leaves the queue (its MESSAGE_EXPIRED receipt to
	// the sender is CR-FEAT-025's tested surface, not re-derived here).
	expiring, expiringBody := deliverToCapability(t, router, capabilityProbe,
		`{"payload":{"job":"expire-me"},"sender":"alice","ttl_seconds":1}`)
	if expiring.Code != http.StatusCreated {
		t.Fatalf("ttl delivery = %d: %s", expiring.Code, expiring.Body.String())
	}
	if expiringBody["target"] != target {
		t.Fatalf("ttl delivery went to %v, want the single holder %s", expiringBody["target"], target)
	}
	time.Sleep(1200 * time.Millisecond)
	store.PurgeExpired()
	if depth, _ := inboxStats(t, router, target); depth != 1 {
		t.Errorf("queue_depth after the TTL expiry = %d, want 1 (only the requeued message remains)", depth)
	}
}

// ---------------------------------------------------------------------------
// The delivery path's own contract: one observation per request (CR-FEAT-030)
// ---------------------------------------------------------------------------

// recordingDetector is a minimal Detector double: it records every observation
// and answers containment from a set.
type recordingDetector struct {
	mu           sync.Mutex
	contained    map[string]bool
	observations []DeliveryObservation
}

func newRecordingDetector() *recordingDetector {
	return &recordingDetector{contained: map[string]bool{}}
}

func (d *recordingDetector) Observe(obs DeliveryObservation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observations = append(d.observations, obs)
}

func (d *recordingDetector) Quarantined(agentID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.contained[agentID]
}

func (d *recordingDetector) seen() []DeliveryObservation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DeliveryObservation(nil), d.observations...)
}

func TestCapabilityDeliveryIsObservedOncePerRequest(t *testing.T) {
	store := setupTestStore(t)
	handler, router := setupRouterWithHandler(store)
	detector := newRecordingDetector()
	handler.detector = detector
	capabilityHolder(t, store, "worker-a", capabilityProbe)

	deliverToCapability(t, router, capabilityProbe, `{"payload":{"job":"routed"},"sender":"alice"}`)
	deliverToCapability(t, router, "nobody-holds-this", `{"payload":{"job":"refused"},"sender":"alice"}`)

	seen := detector.seen()
	if len(seen) != 2 {
		t.Fatalf("observations = %d, want exactly one per delivery request", len(seen))
	}
	if seen[0].Target != "worker-a" || seen[0].Verdict != VerdictDelivered {
		t.Errorf("routed observation = target %q verdict %q, want worker-a/%s", seen[0].Target, seen[0].Verdict, VerdictDelivered)
	}
	if seen[1].Target != "" {
		t.Errorf("refusal observation named a target (%q) — no holder was chosen, so none may be invented", seen[1].Target)
	}
	if seen[1].Verdict != VerdictAgentNotFound {
		t.Errorf("refusal verdict = %q, want %s (the delivery had no target)", seen[1].Verdict, VerdictAgentNotFound)
	}

	// A CONTAINED holder may not receive: the target-side check runs on the
	// RESOLVED holder for a capability-routed request (CR-FEAT-030), which is
	// why it is not skipped just because the sender named a pool.
	before, _ := inboxStats(t, router, "worker-a")
	detector.mu.Lock()
	detector.contained["worker-a"] = true
	detector.mu.Unlock()
	rec, body := deliverToCapability(t, router, capabilityProbe, `{"payload":{"job":"to-a-contained-worker"}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delivery to a contained holder = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if body["error"] != "AGENT_QUARANTINED" {
		t.Errorf("error = %v, want AGENT_QUARANTINED", body["error"])
	}
	if depth, _ := inboxStats(t, router, "worker-a"); depth != before {
		t.Errorf("a contained holder's queue went from %d to %d message(s) — the refusal must store nothing", before, depth)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: the cursor is shared state on the deliver path
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryConcurrentRotationIsFairAndRaceFree(t *testing.T) {
	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)
	holders := []string{"worker-a", "worker-b", "worker-c", "worker-d"}
	for _, id := range holders {
		capabilityHolder(t, store, id, capabilityProbe)
	}

	const deliveries = 40 // 10 turns each
	var (
		mu      sync.Mutex
		counts  = map[string]int{}
		wg      sync.WaitGroup
		failure string
	)
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/capabilities/"+capabilityProbe+"/inbox",
				strings.NewReader(fmt.Sprintf(`{"payload":{"n":%d},"sender":"alice"}`, i)))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			mu.Lock()
			defer mu.Unlock()
			if rec.Code != http.StatusCreated {
				failure = fmt.Sprintf("delivery %d = %d: %s", i, rec.Code, rec.Body.String())
				return
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				failure = fmt.Sprintf("delivery %d body: %v", i, err)
				return
			}
			target, _ := body["target"].(string)
			counts[target]++
		}(i)
	}
	wg.Wait()

	if failure != "" {
		t.Fatal(failure)
	}
	if len(counts) != len(holders) {
		t.Fatalf("concurrent rotation reached %d holder(s), want %d: %v", len(counts), len(holders), counts)
	}
	for _, id := range holders {
		if counts[id] != deliveries/len(holders) {
			t.Errorf("%s took %d of %d deliveries, want %d", id, counts[id], deliveries, deliveries/len(holders))
		}
		if depth, _ := inboxStats(t, router, id); depth != deliveries/len(holders) {
			t.Errorf("%s queue_depth = %d, want %d", id, depth, deliveries/len(holders))
		}
	}
}

// ---------------------------------------------------------------------------
// The webhook branch is the same deliver path, so it names the holder too
// ---------------------------------------------------------------------------

func TestCapabilityDeliveryToAWebhookHolderNamesItInTheAccept(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	store := setupTestStore(t)
	_, pub := newTestPubKey(t)
	if err := store.Register(&Agent{
		ID:           "worker-webhook",
		PublicKey:    HexKey(pub),
		Capabilities: []string{capabilityProbe},
		Webhook: &webhook.Config{
			URL:          endpoint.URL,
			DeliveryMode: "async",
			Retries:      0,
			TimeoutMs:    5000,
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	handler, router := setupRouterWithHandler(store)
	driver := webhook.NewDriver(webhook.NewClient(2*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries:     5,
		RedeliverEvery: 200 * time.Millisecond,
		ProbeEvery:     200 * time.Millisecond,
	})
	driver.Start()
	defer driver.Stop()
	// The queue drain resolves the agent's webhook config through this resolver —
	// without it items are silently dropped ("agent gone").
	driver.SetConfigResolver(func(agentID string) (*webhook.Config, error) {
		agent, err := store.Get(agentID)
		if err != nil {
			return nil, err
		}
		if agent.Webhook == nil {
			return nil, fmt.Errorf("agent %s has no webhook configured", agentID)
		}
		return agent.Webhook, nil
	})
	handler.SetWebhookDriver(driver)

	rec, body := deliverToCapability(t, router, capabilityProbe, `{"payload":{"job":"pushed"},"sender":"alice"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook-holder capability delivery = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if body["transport"] != "webhook" {
		t.Errorf("transport = %v, want webhook", body["transport"])
	}
	if body["capability"] != capabilityProbe || body["target"] != "worker-webhook" {
		t.Errorf("webhook accept = capability %v / target %v, want %s/worker-webhook", body["capability"], body["target"], capabilityProbe)
	}
}
