package registry

// CR-CHAT-020: the presence aggregate must be counted with the SAME Presence
// rule GET /agents reports with — one derivation, so the "N/M agents online"
// figure cannot disagree with the roster it summarizes.

import (
	"testing"
	"time"
)

func TestCountAgentsForStatusUsesTheSamePresenceRule(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store)

	now := time.Now()
	fresh := &Agent{ID: "fresh", PublicKey: HexKey("aa"), Capabilities: []string{}}
	stale := &Agent{ID: "stale", PublicKey: HexKey("bb"), Capabilities: []string{}}
	offline := &Agent{ID: "gone", PublicKey: HexKey("cc"), Capabilities: []string{}, Status: StatusOffline}

	for _, a := range []*Agent{fresh, stale, offline} {
		if err := store.Register(a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
	}
	var regStore Store = store
	// Give `fresh` live evidence; push `stale`'s evidence well outside the
	// default window — the same derivation GET /agents applies. Touch only
	// ADVANCES last_seen (the heartbeat contract), and Register stamps
	// LastSeen=now on every row, so the stale row's evidence is rewritten
	// directly (in-package; a test double of the same fact).
	if err := regStore.(Toucher).Touch("fresh", now); err != nil {
		t.Fatalf("touch fresh: %v", err)
	}
	store.mu.Lock()
	store.agents["stale"].LastSeen = now.Add(-10 * DefaultStalenessWindow)
	store.agents["gone"].LastSeen = now.Add(-10 * DefaultStalenessWindow)
	store.mu.Unlock()

	counts := h.CountAgentsForStatus(now)
	if counts.Total != 3 {
		t.Errorf("Total = %d, want 3", counts.Total)
	}
	if counts.Online != 1 {
		t.Errorf("Online = %d, want 1 (fresh only; stale is outside the window, offline states so itself)", counts.Online)
	}

	// Cross-check against the listing the same handler serves: the aggregate
	// must equal a client-side count over GET /agents rows — otherwise the
	// number is computable two ways.
	listed := h.presence.DeriveAll(store.List(), now)
	clientOnline := 0
	for _, a := range listed {
		if a.Status == StatusOnline {
			clientOnline++
		}
	}
	if clientOnline != counts.Online {
		t.Errorf("aggregate Online = %d but a client recount of the same derivation gets %d — two ways to compute one number", counts.Online, clientOnline)
	}
}
