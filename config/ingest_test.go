package config

import (
	"strings"
	"testing"
)

func TestWebhookIngestDisabledByDefaultAndRequiresStoreWhenEnabled(t *testing.T) {
	get := func(key string) string { return "" }
	cfg, err := LoadFrom(get)
	if err != nil {
		t.Fatalf("LoadFrom defaults: %v", err)
	}
	if cfg.Ingest.Enabled {
		t.Fatal("webhook ingest must default off")
	}
	if cfg.RSS.Enabled || cfg.RSS.MaxItemBytes != 4096 {
		t.Fatalf("RSS defaults must be disabled with 4096-byte item limit: %#v", cfg.RSS)
	}
	_, err = LoadFrom(func(key string) string {
		if key == "CR_INGEST_WEBHOOK_ENABLED" {
			return "true"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CR_INGEST_STORE_FILE") {
		t.Fatalf("enabled configuration error=%v, want CR_INGEST_STORE_FILE", err)
	}
	cfg, err = LoadFrom(func(key string) string {
		switch key {
		case "CR_INGEST_WEBHOOK_ENABLED":
			return "true"
		case "CR_INGEST_STORE_FILE":
			return "ingest.sqlite"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("valid enabled config: %v", err)
	}
	if !cfg.Ingest.Enabled || cfg.Ingest.ReplayWindow.Seconds() != 300 || cfg.Ingest.MaxBodyBytes != 65536 || cfg.Ingest.SourceRatePerMinute != 60 {
		t.Fatalf("unexpected ingest config: %#v", cfg.Ingest)
	}
}
