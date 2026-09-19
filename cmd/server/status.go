package main

import (
	"encoding/json"
	"net/http"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/buildinfo"
)

// DF-CRIER-113: GET /status — the effective runtime posture of this process.
//
// The surfaces crier already had answer narrower questions: /health says the
// process is up, /version says which build it is, and the startup log lines say
// what was configured — once, at boot, and only to whoever was reading the log.
// The gap this endpoint closes is the operator asking a LIVE server "what is
// actually in force right now?": whether auth is enforced, whether per-agent
// signatures are required, whether the message guard is on, and whether the
// registry is in memory or PostgreSQL. Those are exactly the facts a
// misconfiguration hides — a server that answers /health "ok" while running
// wider open (no token, no signature requirement) looks identical to a locked
// one from the outside until a request is rejected.
//
// What it must never do is turn a diagnostic into a disclosure. Every field
// below is an effective BOOLEAN or MODE, never a value: no CR_AUTH_TOKEN, no
// CR_DATABASE_URL (not even redacted — the backend name is the operational
// fact; the connection string, including any password in it, is not), no
// CR_WEBHOOK_SECRET, no CR_FED_TOKEN, no guard provider credentials and no
// guard base URL. A hostname or a filesystem path can leak topology, so the
// federation hold queue reports its durability MODE ("none"/"memory"/"file")
// rather than CR_FED_QUEUE_FILE's path.
//
// Auth: /status is NOT on the exempt list in internal/middleware/auth.go, so
// with CR_AUTH_TOKEN set it requires the Bearer header like every other
// authenticated route; with auth disabled it is open. That asymmetry is
// deliberate — the endpoint reports whether auth is enforced, so it must not
// be readable by an unauthenticated caller on a server that HAS auth on. The
// public half of that question ("is this thing up, and which build?") is what
// /health and /version already answer.

// Registry backend names as they appear in the GET /status body. They are the
// schema's enum values, so a rename is a wire change.
const (
	registryBackendMemory   = "memory"
	registryBackendPostgres = "postgres"
)

// registryBackendForURL is the SINGLE decision point for which registry
// backend serves this process: run() keys the store construction off this same
// helper, so the "registry_backend" GET /status reports is derived from the
// identical input that selected the store. A separate copy of the rule in the
// status path could drift from the selection and report a backend that is not
// the one answering — the exact lie this endpoint exists to prevent.
func registryBackendForURL(dbURL string) string {
	if dbURL != "" {
		return registryBackendPostgres
	}
	return registryBackendMemory
}

// Federation hold-queue modes (DF-CRIER-7): these are the three reachable
// states, not a guess. With no links configured there is no hold path at all;
// with links configured the queue is either the durable file document
// (CR_FED_QUEUE_FILE) or the process-lifetime in-memory one. The PATH itself
// is config, not posture, and is never reported.
const (
	federationHoldQueueNone   = "none"
	federationHoldQueueMemory = "memory"
	federationHoldQueueFile   = "file"
)

// federationHoldQueueMode returns the durability mode of the federation hold
// path in force for cfg.
func federationHoldQueueMode(cfg config.Config) string {
	if len(cfg.Federation.Links) == 0 {
		return federationHoldQueueNone
	}
	if cfg.Federation.QueueFile != "" {
		return federationHoldQueueFile
	}
	return federationHoldQueueMemory
}

// statusResponse is the wire contract of GET /status. Every field is an
// effective boolean or mode — see the file comment for what is deliberately
// absent. The Build object is buildinfo.Info itself, which is the same type
// GET /version serializes, so the two surfaces cannot disagree about which
// build is answering (DF-CRIER-101/127); it is nested rather than flattened so
// the build identity stays one object on both endpoints.
type statusResponse struct {
	// AuthEnabled reports whether a Bearer token is enforced (CR_AUTH_TOKEN
	// non-empty). False is the documented development posture.
	AuthEnabled bool `json:"auth_enabled"`
	// RequireAgentSignature reports whether per-agent ed25519 signing is
	// enforced on agent-scoped routes (CR_REQUIRE_AGENT_SIG, default true).
	RequireAgentSignature bool `json:"require_agent_signature"`
	// GuardEnabled reports the LLM message guard master switch
	// (CR_GUARD_ENABLED, default true).
	GuardEnabled bool `json:"guard_enabled"`
	// RegistryBackend is "memory" or "postgres" — the backend actually
	// serving this process, derived from the same input that selected it.
	RegistryBackend string `json:"registry_backend"`
	// RateLimitPerMinute is the effective relay publish budget; 0 means
	// unlimited (CR_RATE_LIMIT_PER_MINUTE).
	RateLimitPerMinute int `json:"rate_limit_per_minute"`
	// LogLevel and LogFormat are the effective logger settings, so an
	// operator can tell a debug server from an info one without the log.
	LogLevel  string `json:"log_level"`
	LogFormat string `json:"log_format"`
	// WebhookSigning reports whether outbound webhook POSTs are HMAC-signed
	// (CR_WEBHOOK_SECRET set). The secret itself is never serialized.
	WebhookSigning bool `json:"webhook_signing"`
	// FederationEnabled reports whether relay-to-relay links are configured
	// (CR_FED_LINKS). FederationHoldQueue is the durability mode of the hold
	// path in force: "none" (no links), "memory" (process-lifetime) or
	// "file" (survives a restart).
	FederationEnabled   bool   `json:"federation_enabled"`
	FederationHoldQueue string `json:"federation_hold_queue"`
	// MetricsEnabled and PProfEnabled report whether the opt-in inspection
	// surfaces (DF-CRIER-142) are registered. False means the paths answer
	// 404, which is a different posture from "registered but empty".
	MetricsEnabled bool `json:"metrics_enabled"`
	PProfEnabled   bool `json:"pprof_enabled"`
	// Build is the running binary's identity — byte-for-byte the object
	// GET /version serves.
	Build buildinfo.Info `json:"build"`
}

// buildStatusResponse assembles the posture snapshot. It takes the effective
// registry backend as an argument rather than recomputing it: run() passes the
// value it selected the store with, so this response cannot claim a backend
// the process is not using. The build identity is resolved per request, exactly
// as handleVersion does, so the two endpoints read one source.
func buildStatusResponse(cfg config.Config, registryBackend string) statusResponse {
	return statusResponse{
		AuthEnabled:           cfg.AuthToken != "",
		RequireAgentSignature: cfg.RequireAgentSig,
		GuardEnabled:          cfg.Guard.Enabled,
		RegistryBackend:       registryBackend,
		RateLimitPerMinute:    cfg.RateLimitPerMinute,
		LogLevel:              cfg.LogLevel,
		LogFormat:             cfg.LogFormat,
		WebhookSigning:        cfg.Webhook.Secret != "",
		FederationEnabled:     len(cfg.Federation.Links) > 0,
		FederationHoldQueue:   federationHoldQueueMode(cfg),
		MetricsEnabled:        cfg.Observability.EnableMetrics,
		PProfEnabled:          cfg.Observability.EnablePProf,
		Build:                 buildinfo.Resolve(),
	}
}

// newStatusHandler returns the GET /status handler for a booted server. The
// configuration and the selected backend are captured at boot — they cannot
// change while the process serves — so the handler reads no globals and stays
// testable without env plumbing (the same seam registerObservability uses).
//
// Encoding a fixed-shape struct of booleans, small ints, short strings and a
// nested identity cannot fail; a write error here means the client went away,
// which the connection layer already reports (see handleVersion).
func newStatusHandler(cfg config.Config, registryBackend string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(buildStatusResponse(cfg, registryBackend))
	}
}
