package registry

import (
	"testing"

	"github.com/crier-dev/crier/internal/webhook"
)

func TestClassifyConsumerCensusFailsClosed(t *testing.T) {
	agents := []*Agent{
		{ID: "wired", Consumer: &ConsumerContract{Reader: ConsumerReaderHarness}, Webhook: &webhook.Config{URL: "https://consumer.example/hook", DeliveryMode: "blocking", RequireResponseToken: true}},
		{ID: "archived", ArchivedReason: "retired owner; no replacement deployed"},
		{ID: "stale"},
		{ID: "broken", Webhook: &webhook.Config{URL: "not-a-url"}},
	}
	entries := ClassifyConsumerCensus(agents)
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(entries))
	}
	want := map[string]ConsumerCensusState{
		"wired": ConsumerWired, "archived": ConsumerArchived,
		"stale": ConsumerUnclassified, "broken": ConsumerUnclassified,
	}
	for _, entry := range entries {
		if entry.State != want[entry.AgentID] {
			t.Errorf("%s state = %q, want %q (%s)", entry.AgentID, entry.State, want[entry.AgentID], entry.Reason)
		}
		if entry.Reason == "" {
			t.Errorf("%s has no classification reason", entry.AgentID)
		}
	}

	if err := ValidateConsumerCensus(agents); err == nil || !contains(err.Error(), "stale") || !contains(err.Error(), "broken") {
		t.Fatalf("ValidateConsumerCensus error = %v, want both unclassified rows", err)
	}
	if err := ValidateConsumerCensus(agents[:2]); err != nil {
		t.Fatalf("wired and archived census rejected: %v", err)
	}
}
