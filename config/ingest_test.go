package config

import (
	"strings"
	"testing"
	"time"
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
	if cfg.RSS.Enabled || cfg.RSS.MaxItemBytes != 4096 || cfg.RSS.ConfigFile != "config/rss-feeds.json" {
		t.Fatalf("RSS defaults must be disabled with checked-in config and 4096-byte item limit: %#v", cfg.RSS)
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

// TestReconciliationSweepPosture pins the INT-015 sweep config posture
// (CRIER-GITHOOK-DISPATCH §10): opt-in, fail-closed, tolerant-bool, cadenced.
func TestReconciliationSweepPosture(t *testing.T) {
	cfg, err := LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Ingest.SweepEnabled {
		t.Fatal("reconciliation sweep must default off (fail-closed)")
	}
	// Enabling the sweep alone refuses startup: the sweep drains the ingest
	// store, so an armed sweep without the ingest surface is a lie.
	_, err = LoadFrom(func(key string) string {
		if key == "CR_INGEST_SWEEP_ENABLED" {
			return "true"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CR_INGEST_WEBHOOK_ENABLED") {
		t.Fatalf("sweep without ingest surface: error=%v, want the CR_INGEST_WEBHOOK_ENABLED requirement", err)
	}
	cfg, err = LoadFrom(func(key string) string {
		switch key {
		case "CR_INGEST_WEBHOOK_ENABLED":
			return "true"
		case "CR_INGEST_STORE_FILE":
			return "ingest.sqlite"
		case "CR_INGEST_SWEEP_ENABLED":
			return "yes"
		case "CR_INGEST_SWEEP_INTERVAL_SECS":
			return "30"
		case "CR_INGEST_SWEEP_BOARD_DIR":
			return "/srv/board"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("valid sweep config: %v", err)
	}
	if !cfg.Ingest.SweepEnabled || cfg.Ingest.SweepInterval != 30*time.Second || cfg.Ingest.SweepBoardDir != "/srv/board" {
		t.Fatalf("sweep config: %#v", cfg.Ingest)
	}
	// A broken value fails loudly (the tolerant-bool dialect never guesses).
	_, err = LoadFrom(func(key string) string {
		switch key {
		case "CR_INGEST_WEBHOOK_ENABLED":
			return "true"
		case "CR_INGEST_STORE_FILE":
			return "ingest.sqlite"
		case "CR_INGEST_SWEEP_ENABLED":
			return "yes"
		case "CR_INGEST_SWEEP_INTERVAL_SECS":
			return "soon"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CR_INGEST_SWEEP_INTERVAL_SECS") {
		t.Fatalf("bad interval: error=%v, want CR_INGEST_SWEEP_INTERVAL_SECS", err)
	}
	_, err = LoadFrom(func(key string) string {
		switch key {
		case "CR_INGEST_WEBHOOK_ENABLED":
			return "true"
		case "CR_INGEST_STORE_FILE":
			return "ingest.sqlite"
		case "CR_INGEST_SWEEP_ENABLED":
			return "maybe"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CR_INGEST_SWEEP_ENABLED") {
		t.Fatalf("bad switch: error=%v, want CR_INGEST_SWEEP_ENABLED", err)
	}
	// Unset interval resolves to the spec cadence, 10 minutes.
	cfg, err = LoadFrom(func(key string) string {
		switch key {
		case "CR_INGEST_WEBHOOK_ENABLED":
			return "true"
		case "CR_INGEST_STORE_FILE":
			return "ingest.sqlite"
		}
		return ""
	})
	if err != nil || cfg.Ingest.SweepInterval != 10*time.Minute {
		t.Fatalf("default sweep interval: %#v err=%v, want 600s", cfg.Ingest.SweepInterval, err)
	}
}
