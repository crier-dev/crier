package registry

import (
	"testing"

	"github.com/crier-dev/crier/internal/webhook"
)

func TestClassifyConsumerCensusFailsClosed(t *testing.T) {
	agents := []*Agent{
		{ID: "wired", Consumer: &ConsumerContract{Reader: ConsumerReaderHarness}, Webhook: &webhook.Config{URL: "https://consumer.example/hook", DeliveryMode: "blocking", RequireResponseToken: true}},
		{ID: "async-wired", Consumer: &ConsumerContract{Reader: ConsumerReaderHarness}, Webhook: &webhook.Config{URL: "https://consumer.example/async", DeliveryMode: "async"}},
		{ID: "blocking-unproven", Consumer: &ConsumerContract{Reader: ConsumerReaderHarness}, Webhook: &webhook.Config{URL: "https://consumer.example/no-token", DeliveryMode: "blocking"}},
		{ID: "consumer-without-webhook", Consumer: &ConsumerContract{Reader: ConsumerReaderHarness}},
		{ID: "invalid-webhook", Consumer: &ConsumerContract{Reader: ConsumerReaderHarness}, Webhook: &webhook.Config{URL: "not-a-url"}},
		{ID: "archived", ArchivedReason: "retired owner; no replacement deployed"},
		{ID: "stale"},
		{ID: "broken", Webhook: &webhook.Config{URL: "not-a-url"}},
	}
	entries := ClassifyConsumerCensus(agents)
	if len(entries) != len(agents) {
		t.Fatalf("entries = %d, want %d", len(entries), len(agents))
	}
	want := map[string]ConsumerCensusState{
		"wired": ConsumerWired, "async-wired": ConsumerWired,
		"blocking-unproven": ConsumerUnclassified, "consumer-without-webhook": ConsumerUnclassified,
		"invalid-webhook": ConsumerUnclassified, "archived": ConsumerArchived,
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
	if err := ValidateConsumerCensus([]*Agent{agents[0], agents[1], agents[5]}); err != nil {
		t.Fatalf("wired and archived census rejected: %v", err)
	}

	entries = ClassifyConsumerCensus([]*Agent{agents[0], agents[1], agents[5]})
	if entries[0].State != ConsumerWired || entries[1].State != ConsumerWired || entries[2].State != ConsumerArchived {
		t.Fatalf("wired/archived entrypoint classification = %+v", entries)
	}
}
