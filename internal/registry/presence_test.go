package registry

// CR-FEAT-024 — presence: the status a registry row REPORTS is derived from its
// liveness evidence (last_seen) and one documented window, so an agent whose
// process died stops being reported `online`.
//
// What these tests pin, and why each one is worth having:
//
//   - the derivation table, INCLUDING the boundary (evidence exactly as old as
//     the window is still online) — an off-by-one here would flap a healthy
//     agent;
//   - a stated `offline` outranks the derivation, so a row that says it is off
//     is never reported online just because something touched it;
//   - the derivation is a READ: it never writes the stored row (which is what
//     keeps the agents.status CHECK constraint — online/offline — honest, and
//     stops one read from changing what the next read sees);
//   - the two HTTP read surfaces derive (GET /agents and GET /agents/{id}),
//     because a rule that never reaches the wire is not a fix;
//   - Touch advances last_seen monotonically and never touches anything else;
//   - HeartbeatSink is nil for a store that cannot touch, and never panics for
//     a heartbeat from an agent that has no row.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// agedRow registers an agent and then rewinds its last_seen, so a test can put
// a row in the past without sleeping. Reaching into the store's own map is
// deliberate: this is an in-package test, and it is the only way to build a
// genuinely old row that the public API cannot produce (Register stamps now,
// and Touch is monotonic).
func agedRow(t *testing.T, store *MemoryStore, id string, age time.Duration) *Agent {
	t.Helper()
	if err := store.Register(&Agent{ID: id, Capabilities: []string{"relay"}}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	row := store.agents[id]
	row.LastSeen = time.Now().Add(-age)
	return row
}

func TestPresenceStatusTable(t *testing.T) {
	window := DefaultStalenessWindow
	now := time.Now()

	cases := []struct {
		name     string
		stored   AgentStatus
		lastSeen time.Time
		want     AgentStatus
	}{
		{"fresh evidence is online", StatusOnline, now.Add(-time.Second), StatusOnline},
		{"evidence exactly at the window is still online", StatusOnline, now.Add(-window), StatusOnline},
		{"evidence one tick past the window is stale", StatusOnline, now.Add(-window - time.Millisecond), StatusStale},
		{"a row that was never seen is stale, not online", StatusOnline, time.Time{}, StatusStale},
		{"a stated offline outranks fresh evidence", StatusOffline, now, StatusOffline},
		{"a stated offline outranks no evidence at all", StatusOffline, time.Time{}, StatusOffline},
		{"an empty stored status derives from evidence", "", now, StatusOnline},
		{"an empty stored status with old evidence is stale", "", now.Add(-time.Hour), StatusStale},
	}

	p := NewPresence(window)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.StatusOf(&Agent{ID: "a", Status: tc.stored, LastSeen: tc.lastSeen}, now)
			if got != tc.want {
				t.Errorf("StatusOf(status=%q, last_seen=%v) = %q, want %q",
					tc.stored, tc.lastSeen, got, tc.want)
			}
		})
	}

	if p.StaleAfter() != window {
		t.Errorf("StaleAfter() = %v, want the configured window %v", p.StaleAfter(), window)
	}
}

// TestPresenceWindowDefaults pins the fail-safe direction of a zero/negative
// window: it means the DOCUMENTED DEFAULT, never "every row is stale". A window
// of 0 taken literally would mark a row stale the instant its last heartbeat
// was a millisecond old, which reads as a fleet-wide outage.
func TestPresenceWindowDefaults(t *testing.T) {
	for _, window := range []time.Duration{0, -time.Second} {
		if got := NewPresence(window).StaleAfter(); got != DefaultStalenessWindow {
			t.Errorf("NewPresence(%v).StaleAfter() = %v, want the default %v",
				window, got, DefaultStalenessWindow)
		}
	}
	// The zero value of the struct must behave identically: a Handler or an
	// embedder that never configured presence still derives honestly.
	var zero Presence
	if got := zero.StaleAfter(); got != DefaultStalenessWindow {
		t.Errorf("zero-value Presence.StaleAfter() = %v, want the default %v", got, DefaultStalenessWindow)
	}
	if got := NewPresence(5 * time.Second).StaleAfter(); got != 5*time.Second {
		t.Errorf("NewPresence(5s).StaleAfter() = %v, want 5s", got)
	}
	if got := NewPresence(5*time.Second).StatusOf(&Agent{LastSeen: time.Now().Add(-6 * time.Second)}, time.Now()); got != StatusStale {
		t.Errorf("a 6s-old row under a 5s window = %q, want %q", got, StatusStale)
	}
}

// TestPresenceDeriveDoesNotMutateTheStoredRow is the read-purity property the
// Postgres CHECK constraint and every honest consumer depend on: the derivation
// reports `stale` for an old row while the row itself keeps whatever it stored.
func TestPresenceDeriveDoesNotMutateTheStoredRow(t *testing.T) {
	store := NewMemoryStore()
	row := agedRow(t, store, "walker", time.Hour)
	storedSeen := row.LastSeen

	derived := NewPresence(0).Derive(row, time.Now())
	if derived.Status != StatusStale {
		t.Fatalf("derived status = %q, want %q", derived.Status, StatusStale)
	}
	if derived == row {
		t.Fatal("Derive returned the stored pointer — a derived value would then be written into the row")
	}
	if row.Status != StatusOnline {
		t.Errorf("stored status = %q after a read, want it untouched at %q", row.Status, StatusOnline)
	}
	if !row.LastSeen.Equal(storedSeen) {
		t.Errorf("stored last_seen = %v after a read, want it untouched at %v", row.LastSeen, storedSeen)
	}

	// The rule re-derives on every read, so a row that receives fresh evidence
	// is online again — the derivation is not a one-way latch.
	if got := NewPresence(0).StatusOf(row, time.Now()); got != StatusStale {
		t.Errorf("re-derived status = %q, want %q while the evidence is still old", got, StatusStale)
	}
	if err := store.Touch("walker", time.Now()); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if got := NewPresence(0).StatusOf(store.agents["walker"], time.Now()); got != StatusOnline {
		t.Errorf("status after fresh evidence = %q, want %q", got, StatusOnline)
	}
}

func TestPresenceDeriveNilAndDeriveAll(t *testing.T) {
	p := NewPresence(0)
	if got := p.Derive(nil, time.Now()); got != nil {
		t.Errorf("Derive(nil) = %v, want nil", got)
	}
	if got := p.DeriveAll(nil, time.Now()); got != nil {
		t.Errorf("DeriveAll(nil) = %v, want nil", got)
	}
	old := &Agent{ID: "old", Status: StatusOnline, LastSeen: time.Now().Add(-time.Hour)}
	fresh := &Agent{ID: "fresh", Status: StatusOnline, LastSeen: time.Now()}
	got := p.DeriveAll([]*Agent{old, fresh}, time.Now())
	if len(got) != 2 {
		t.Fatalf("DeriveAll returned %d rows, want 2", len(got))
	}
	if got[0].Status != StatusStale || got[1].Status != StatusOnline {
		t.Errorf("DeriveAll statuses = %q/%q, want stale/online", got[0].Status, got[1].Status)
	}
	// One instant for the whole listing: a slow store must not judge the first
	// row of a GET /agents body by a different clock than the last.
	if !got[0].LastSeen.Equal(old.LastSeen) || !got[1].LastSeen.Equal(fresh.LastSeen) {
		t.Error("DeriveAll changed a last_seen — it must only derive the status")
	}
}

// TestHandlersReportStaleStatus drives the two HTTP read surfaces through the
// real handlers: the derived value is what a dashboard built on GET /agents or
// GET /agents/{id} actually receives.
func TestHandlersReportStaleStatus(t *testing.T) {
	store := NewMemoryStore()
	agedRow(t, store, "walker", time.Hour)

	h := NewHandler(store)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods("GET")

	// GET /agents/{id}
	code, body := doHandlerGet(t, r, "/agents/walker")
	if code != http.StatusOK {
		t.Fatalf("GET /agents/walker = %d: %s", code, body)
	}
	var one Agent
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decode agent detail %s: %v", body, err)
	}
	if one.Status != StatusStale {
		t.Errorf("GET /agents/walker status = %q, want %q", one.Status, StatusStale)
	}

	// GET /agents
	code, body = doHandlerGet(t, r, "/agents")
	if code != http.StatusOK {
		t.Fatalf("GET /agents = %d: %s", code, body)
	}
	var list struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode agent list %s: %v", body, err)
	}
	if len(list.Agents) != 1 || list.Agents[0].Status != StatusStale {
		t.Fatalf("GET /agents = %s, want one agent reporting %q", body, StatusStale)
	}

	// The read did not rewrite the row...
	if store.agents["walker"].Status != StatusOnline {
		t.Fatalf("the stored row was rewritten by a read: status = %q", store.agents["walker"].Status)
	}

	// ...and a heartbeat now makes both surfaces report online again.
	if err := store.Touch("walker", time.Now()); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	code, body = doHandlerGet(t, r, "/agents/walker")
	if code != http.StatusOK {
		t.Fatalf("GET /agents/walker after a heartbeat = %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decode agent detail %s: %v", body, err)
	}
	if one.Status != StatusOnline {
		t.Errorf("status after a heartbeat = %q, want %q", one.Status, StatusOnline)
	}
}

// doHandlerGet drives one GET through a router and returns status + body.
func doHandlerGet(t *testing.T, r *mux.Router, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestMemoryStoreTouchAdvancesLastSeenOnly(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "agent-a", Capabilities: []string{"relay"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Make the row's stored status something other than the registration
	// default, so "Touch changed nothing but last_seen" is observable.
	store.agents["agent-a"].Status = StatusOffline

	heartbeat := time.Now().Add(time.Minute)
	if err := store.Touch("agent-a", heartbeat); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	row := store.agents["agent-a"]
	if !row.LastSeen.Equal(heartbeat) {
		t.Errorf("last_seen = %v, want the heartbeat instant %v", row.LastSeen, heartbeat)
	}
	if row.Status != StatusOffline {
		t.Errorf("stored status = %q, want it untouched at %q (Touch dates a row, it does not restate it)",
			row.Status, StatusOffline)
	}
	if len(row.Capabilities) != 1 || row.Capabilities[0] != "relay" {
		t.Errorf("capabilities = %v, want the registered row untouched", row.Capabilities)
	}

	// Monotonic: an out-of-order (older) heartbeat must not move the row
	// backwards — that would make a live agent look older than its evidence.
	if err := store.Touch("agent-a", heartbeat.Add(-time.Hour)); err != nil {
		t.Fatalf("Touch (older): %v", err)
	}
	if !store.agents["agent-a"].LastSeen.Equal(heartbeat) {
		t.Errorf("last_seen = %v after an older heartbeat, want it held at %v",
			store.agents["agent-a"].LastSeen, heartbeat)
	}

	// A zero instant means "now", never "the zero time".
	if err := store.Touch("agent-a", time.Time{}); err != nil {
		t.Fatalf("Touch (zero): %v", err)
	}
	if store.agents["agent-a"].LastSeen.Before(heartbeat) {
		t.Errorf("last_seen = %v after a zero-instant heartbeat, want it at or after %v",
			store.agents["agent-a"].LastSeen, heartbeat)
	}
}

func TestMemoryStoreTouchUnknownAgent(t *testing.T) {
	store := NewMemoryStore()
	err := store.Touch("ghost", time.Now())
	if err == nil {
		t.Fatal("Touch on an unknown agent returned nil, want ErrAgentNotFound")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("Touch error = %v, want it to wrap ErrAgentNotFound", err)
	}
}

// storeWithoutTouch is a Store that does NOT implement Toucher: embedding the
// Store INTERFACE promotes only the interface's own methods, so Touch (a
// separate optional capability) is deliberately absent, exactly like the remote
// proxy the bridge builds.
type storeWithoutTouch struct{ Store }

func TestHeartbeatSinkIsNilWithoutTheCapability(t *testing.T) {
	var plain Store = storeWithoutTouch{NewMemoryStore()}
	if sink := HeartbeatSink(plain); sink != nil {
		t.Error("HeartbeatSink returned a sink for a store that cannot touch — the mesh would call a nil-capability store")
	}
	// A store that CAN touch gets a sink, and the sink records the evidence.
	local := NewMemoryStore()
	if err := local.Register(&Agent{ID: "agent-a"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sink := HeartbeatSink(local)
	if sink == nil {
		t.Fatal("HeartbeatSink returned nil for MemoryStore")
	}
	at := time.Now().Add(2 * time.Second)
	sink("agent-a", at)
	if !local.agents["agent-a"].LastSeen.Equal(at) {
		t.Errorf("last_seen = %v after the sink ran, want %v", local.agents["agent-a"].LastSeen, at)
	}

	// A heartbeat from an agent with no row is the ORDINARY case (a peer that
	// connected without registering) and must be a silent no-op, not a panic.
	sink("never-registered", time.Now())
	sink("", time.Now())

	// No sink at all (the mesh's default) means the mesh records nothing.
	if sink := HeartbeatSink(nil); sink != nil {
		t.Error("HeartbeatSink(nil) returned a sink")
	}
}
