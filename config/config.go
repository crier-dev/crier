package config

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/detect"
)

// DatabaseConfig holds PostgreSQL connection and pool settings.
type DatabaseConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// Config holds all Crier configuration.
type Config struct {
	Port               int
	AuthToken          string
	Database           DatabaseConfig
	LogLevel           string
	LogFormat          string
	RateLimitPerMinute int
	// InboxMaxBodyBytes bounds the raw request body accepted by POST
	// /agents/{id}/inbox and POST /capabilities/{capability}/inbox
	// (CR_INBOX_MAX_BODY_BYTES). Values must be positive; the default is 1 MiB.
	InboxMaxBodyBytes int
	// GlobalRateLimitPerMinute is the global inbox-ingest budget
	// (CR_RATE_LIMIT_GLOBAL_PER_MINUTE, CR-FEAT-035), in deliveries per minute
	// across every agent. 0 — the default — disables it entirely: the
	// per-agent publish cap (RateLimitPerMinute) stays the only backpressure
	// and the delivery path behaves exactly as it did before the feature
	// existed. When set, a delivery over budget is refused 429 with a named
	// error and a Retry-After, before any transport or store work.
	GlobalRateLimitPerMinute int
	WSAllowedOrigins         string
	// RequireAgentSig enforces per-agent ed25519 request signing on inbox
	// read/ack/stats and agent deletion. Secure by default.
	RequireAgentSig bool
	// RequireMeshAuth enforces the ed25519 connect handshake on the mesh
	// (CR_REQUIRE_MESH_AUTH, DF-CRIER-287). Default FALSE — the mesh is
	// opt-in, because its lane is used by single-host deployments whose
	// clients have no key setup, and tightening the default would break them
	// silently. See docs/mesh-protocol.md §Authentication for the migration
	// note: turning it on requires every connecting agent to hold the private
	// half of its registered key.
	RequireMeshAuth bool
	// MeshAuthTimeout bounds the mesh connect challenge/response
	// (CR_MESH_AUTH_TIMEOUT_S). Zero means "the mesh package's own default"
	// (mesh.DefaultMeshAuthTimeout, 10s) — the value is resolved once, in the
	// mesh config the server builds, so the constant is never duplicated here.
	MeshAuthTimeout time.Duration
	// MeshAllowedOrigins is the mesh-only WebSocket origin allowlist
	// (CR_MESH_ALLOWED_ORIGINS, comma-separated, "*" = allow all). Empty — the
	// default — allows every origin, exactly as before. When set, an upgrade
	// that CARRIES an Origin header must name a listed origin; one that
	// carries NONE is allowed, because a non-browser mesh client (every
	// shipped one) sends no Origin and an allowlist that locked those out
	// would be an outage, not a policy. See config.BuildMeshCheckOrigin.
	MeshAllowedOrigins string
	// PresenceStaleAfter is how long a registry row may go without liveness
	// evidence before the status reported for it becomes "stale"
	// (CR_PRESENCE_STALE_AFTER_S, CR-FEAT-024). Zero means "the registry
	// package's own default" (registry.DefaultStalenessWindow, 90s = three
	// missed mesh heartbeats) — resolved once, where the presence rule is
	// built, so the constant is never duplicated here. Values are
	// validated at load: a non-positive or non-integer value is a startup
	// error, never a silently ignored setting.
	PresenceStaleAfter time.Duration
	// IdempotencyWindow is the window within which a sender-supplied
	// idempotency key deduplicates a delivery (CR_IDEMPOTENCY_WINDOW_S,
	// CR-FEAT-025). Zero means "the registry package's own default"
	// (registry.DefaultIdempotencyWindow, 24h) — resolved once, where the
	// handler's replay registry is built, so the constant is never duplicated
	// here. A non-integer or non-positive value is a startup error, never a
	// silently ignored setting: a window of 0 would mean "deduplicate nothing"
	// while the API still promised the feature.
	IdempotencyWindow time.Duration
	// Webhook holds push-delivery tuning (specs/WEBHOOK-DELIVERY.md §9).
	Webhook WebhookConfig
	// Federation holds relay-to-relay link configuration (CR-FEAT-006).
	Federation FederationConfig
	// Guard holds LLM message-guard tuning (specs/LLM-MESSAGE-GUARD.md
	// §9.1, CR-FEAT-010).
	Guard GuardConfig
	// Observability holds the opt-in live-inspection surfaces
	// (DF-CRIER-142). Both default to false: with the flags unset the
	// server registers neither /metrics nor /debug/pprof and both answer
	// 404 like any unregistered path.
	Observability ObservabilityConfig
	// A2AEnabled is the server-side half of the OPT-IN A2A interoperability
	// gate (CR_A2A_ENABLED, INT-A2A-001, specs/A2A-OPTION.md §4.1). Default
	// false: with the flag unset the option is OFF and NOTHING A2A-related is
	// registered, so every existing route, body, auth requirement and
	// storage path behaves exactly as it did before the option existed. The
	// other half is the per-agent `a2a` block on a registry row (§4.2);
	// either half alone is inert.
	//
	// Consumed by cmd/server (INT-A2A-002): with it set, the server registers
	// the A2A Agent Card discovery route and serves a card for agents whose
	// row opted in — and nothing else. Every other A2A surface is still absent
	// (§5.2), and with the flag unset even that one route does not exist.
	A2AEnabled bool
	// Detection holds the OPT-IN detection & containment layer
	// (CR_DETECT_ENABLED, CR-FEAT-030, specs/DETECTION.md). Default off: with
	// the flag unset the server registers no detection route, writes no
	// delivery log and the delivery path is byte-identical to a build without
	// this feature.
	Detection DetectionConfig
	// Namespaces holds the realm policy set (CR_NAMESPACES /
	// CR_NAMESPACES_FILE, CR-FEAT-029, specs/NAMESPACES.md). Both default to
	// empty, which is a deployment with exactly one implicit namespace —
	// every behaviour this server had before the feature existed.
	Namespaces NamespaceConfig
}

// NamespaceConfig carries the realm policy document as RAW input. Like
// GuardConfig.DefaultPolicy, it is parsed and validated by the owning package
// (internal/namespace) at startup, so a broken policy fails the boot instead of
// silently serving a weaker realm than the operator declared — and so no
// validation rule is duplicated here.
type NamespaceConfig struct {
	// Inline is CR_NAMESPACES: the namespace document itself
	// ({"namespaces":[{...}]}).
	Inline string
	// File is CR_NAMESPACES_FILE: a path to the same document. Set at most
	// ONE of the two — Load refuses both, because "which one won" is not a
	// question an operator should have to answer from a log line.
	File string
}

// DetectionConfig holds the detection layer's tuning (CR-FEAT-030). Every
// zero value falls back to the documented default in internal/detect, so the
// defaults live in ONE place and this struct never duplicates a number.
type DetectionConfig struct {
	// Enabled is CR_DETECT_ENABLED — the master switch.
	Enabled bool
	// LogPath is CR_DETECT_LOG: the append-only signed delivery log. Empty
	// means no log is written (alerts and containment still work, in memory).
	LogPath string
	// KeyPath is CR_DETECT_KEY: the ed25519 signing-key file. When a log path
	// is set and this is empty, the log is signed with "<log path>.key".
	KeyPath string
	// FanoutWindow / FanoutMinTargets are the fan-out signal's window and
	// distinct-target threshold (CR_DETECT_FANOUT_WINDOW_S,
	// CR_DETECT_FANOUT_MIN_TARGETS). Zero = internal/detect's default.
	FanoutWindow     time.Duration
	FanoutMinTargets int
	// NewPeerWindow / NewPeerMinTargets are the new-peer burst signal's
	// window and threshold (CR_DETECT_NEWPEER_WINDOW_S,
	// CR_DETECT_NEWPEER_MIN_TARGETS).
	NewPeerWindow     time.Duration
	NewPeerMinTargets int
	// QuietStartHour / QuietEndHour are the UTC hour bounds of the quiet
	// window (CR_DETECT_QUIET_HOURS, "S-E", start inclusive, end exclusive;
	// a start above the end wraps midnight). config.Load applies the
	// documented 01:00–05:00 default; an EXPLICIT equal pair (0-0) disables
	// the odd-hour signal, and a zero-value Config — built in a test, not by
	// Load — leaves it off too rather than guessing.
	QuietStartHour int
	QuietEndHour   int
	// QuietMinMessages is the odd-hour volume threshold
	// (CR_DETECT_QUIET_MIN_MESSAGES).
	QuietMinMessages int
	// CanaryTokens are the operator-planted canary tokens
	// (CR_CANARY_TOKENS, comma-separated). Empty = the server generates
	// default canaries at boot and logs them.
	CanaryTokens []string
}

// ObservabilityConfig holds the opt-in live-inspection surfaces
// (DF-CRIER-142). Neither surface is auth-exempt: with CR_AUTH_TOKEN set
// they require the Bearer header like every other authenticated route;
// with auth off they are open. The exempt-path list in
// internal/middleware/auth.go is unchanged.
type ObservabilityConfig struct {
	// EnablePProf registers GET /debug/pprof/* (net/http/pprof) when true
	// (CR_ENABLE_PPROF). Default false.
	EnablePProf bool
	// EnableMetrics registers GET /metrics (Prometheus text format) when
	// true (CR_ENABLE_METRICS). Default false.
	EnableMetrics bool
}

// GuardConfig holds LLM message-guard tuning (spec §9.1, CR-FEAT-010).
type GuardConfig struct {
	Enabled          bool          // CR_GUARD_ENABLED — master switch
	Timeout          time.Duration // CR_GUARD_TIMEOUT_MS — per-message budget (all providers, retries included)
	MaxConcurrent    int           // CR_GUARD_MAX_CONCURRENT
	CircuitThreshold int           // CR_GUARD_CIRCUIT_THRESHOLD
	CircuitCooldown  time.Duration // CR_GUARD_CIRCUIT_COOLDOWN_S
	MaxPayloadBytes  int           // CR_GUARD_MAX_PAYLOAD_BYTES — above this the LLM is skipped (§6.3)
	RenderMaxBytes   int           // CR_GUARD_RENDER_MAX_BYTES — projection cap fed to the LLM
	DeepSeekBaseURL  string        // CR_GUARD_DEEPSEEK_BASE_URL — deepseek preset base URL override
	Model            string        // CR_GUARD_MODEL — deepseek preset default model override
	ExtraPatterns    string        // CR_GUARD_PATTERNS_EXTRA — JSON array of extra prematch patterns
	DefaultPolicy    string        // CR_GUARD_DEFAULT_POLICY — JSON Policy (server-wide default, §4.2 step 3 / §9.1)
	KanbanQueueSize  int           // CR_GUARD_KANBAN_QUEUE — kanban worker queue capacity (spec §8.2, CR-FEAT-014)
	KanbanURL        string        // CR_GUARD_KANBAN_URL — HTTP kanban sink base URL ("" = Hermes kanban CLI writer, CR-FEAT-009)
}

// FederationConfig holds relay-to-relay federation settings (CR-FEAT-006).
type FederationConfig struct {
	// Links are the base URLs of linked relays (CR_FED_LINKS, comma-
	// separated). Deliveries to agents unknown on this relay are forwarded
	// to each link in order; GET /fed/peers lists the links with their
	// agents.
	Links []string
	// Name is the optional display name of this relay in the /fed/peers
	// listing (CR_FED_NAME). Defaults to localhost:<port>.
	Name string
	// Token is the optional shared secret for outbound federation link
	// authentication (CR_FED_TOKEN, DF-CRIER-6). When set, every request
	// this relay sends to a linked relay carries
	// "Authorization: Bearer <token>", so a remote relay protecting itself
	// with CR_AUTH_TOKEN accepts the forward. The linked relay must share
	// the value: source CR_FED_TOKEN == destination CR_AUTH_TOKEN. Empty
	// (default) sends no Authorization header — links are unauthenticated,
	// as before. The secret is never logged, echoed, serialized, or
	// included in GET /fed/peers output.
	Token string
	// MaxHold bounds how long a delivery to a remote agent is held at this
	// relay when every link fails transiently, before the sender gets an
	// explicit FEDERATION_FAILED outcome (CR_FED_MAX_HOLD_S, DF-CRIER-7,
	// specs/WEBHOOK-DELIVERY.md §8/§9). Default 300s. Only the transient
	// case is held: a definitive all-links-404 is answered immediately.
	MaxHold time.Duration
	// QueueFile is the path of the durable hold queue document
	// (CR_FED_QUEUE_FILE, DF-CRIER-7). When set, held deliveries survive a
	// source-relay restart. Empty (default) keeps the queue in process
	// memory only — held deliveries are lost on restart, the same contract
	// the in-memory registry backend documents for inboxes.
	QueueFile string
}

// WebhookConfig holds push-delivery tuning (CR-FEAT-001/005).
type WebhookConfig struct {
	Secret             string        // CR_WEBHOOK_SECRET — HMAC outbound signing
	Timeout            time.Duration // CR_WEBHOOK_TIMEOUT_S
	MaxRetries         int           // CR_WEBHOOK_MAX_RETRIES
	RedeliverEvery     time.Duration // CR_WEBHOOK_REDELIVER_S
	ProbeEvery         time.Duration // CR_WEBHOOK_PROBE_S
	CircuitThreshold   int           // CR_WEBHOOK_CIRCUIT_THRESHOLD
	BatchMaxMessages   int           // CR_WEBHOOK_BATCH_MAX — batch flush size default
	BatchFlushInterval time.Duration // CR_WEBHOOK_BATCH_FLUSH_S — batch flush interval default
}

// Load reads configuration from environment with defaults.
func Load() (Config, error) {
	cfg := Config{
		Port:               8767,
		AuthToken:          os.Getenv("CR_AUTH_TOKEN"),
		LogLevel:           "info",
		LogFormat:          "text",
		RateLimitPerMinute: 100,
		InboxMaxBodyBytes:  1 << 20,
		RequireAgentSig:    true,
		Webhook: WebhookConfig{
			Timeout:            30 * time.Second,
			MaxRetries:         5,
			RedeliverEvery:     30 * time.Second,
			ProbeEvery:         60 * time.Second,
			CircuitThreshold:   10,
			BatchMaxMessages:   10,
			BatchFlushInterval: 5 * time.Second,
		},
		Guard: GuardConfig{
			Enabled:          true,
			Timeout:          10 * time.Second,
			MaxConcurrent:    8,
			CircuitThreshold: 10,
			CircuitCooldown:  300 * time.Second,
			MaxPayloadBytes:  65536,
			RenderMaxBytes:   32768,
			DeepSeekBaseURL:  "https://api.deepseek.com/v1",
			Model:            "deepseek-v4-flash",
			KanbanQueueSize:  100,
		},
		Federation: FederationConfig{
			// CR_FED_MAX_HOLD_S (DF-CRIER-7, spec §8/§9).
			MaxHold: 300 * time.Second,
		},
		Database: DatabaseConfig{
			MaxConns:        4,
			MinConns:        0,
			MaxConnLifetime: 30 * time.Minute,
			MaxConnIdleTime: 5 * time.Minute,
			ConnectTimeout:  10 * time.Second,
		},
	}

	if v := os.Getenv("CR_LOG_LEVEL"); v != "" {
		switch v {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = v
		default:
			return cfg, fmt.Errorf("invalid CR_LOG_LEVEL: %q (want debug/info/warn/error)", v)
		}
	}

	if v := os.Getenv("CR_LOG_FORMAT"); v != "" {
		switch v {
		case "text", "json":
			cfg.LogFormat = v
		default:
			return cfg, fmt.Errorf("invalid CR_LOG_FORMAT: %q (want text/json)", v)
		}
	}

	// Port
	if v := os.Getenv("CRIER_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return cfg, fmt.Errorf("invalid CRIER_PORT: %q", v)
		}
		cfg.Port = p
	}

	// Database URL — three env var precedence: CR_DATABASE_URL → DATABASE_URL → CRIER_DATABASE_URL
	dbURL := os.Getenv("CR_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = os.Getenv("CRIER_DATABASE_URL")
	}
	cfg.Database.URL = dbURL

	// Pool settings
	if v := os.Getenv("CR_DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MAX_CONNS: %q", v)
		}
		cfg.Database.MaxConns = int32(n)
	}
	if v := os.Getenv("CR_DATABASE_MIN_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MIN_CONNS: %q", v)
		}
		cfg.Database.MinConns = int32(n)
	}
	if cfg.Database.MinConns > cfg.Database.MaxConns {
		return cfg, fmt.Errorf("CR_DATABASE_MIN_CONNS (%d) > CR_DATABASE_MAX_CONNS (%d)", cfg.Database.MinConns, cfg.Database.MaxConns)
	}

	if v := os.Getenv("CR_DATABASE_MAX_CONN_LIFETIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MAX_CONN_LIFETIME: %q", v)
		}
		cfg.Database.MaxConnLifetime = d
	}
	if v := os.Getenv("CR_DATABASE_MAX_CONN_IDLE_TIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MAX_CONN_IDLE_TIME: %q", v)
		}
		cfg.Database.MaxConnIdleTime = d
	}
	if v := os.Getenv("CR_DATABASE_CONNECT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_CONNECT_TIMEOUT: %q", v)
		}
		cfg.Database.ConnectTimeout = d
	}

	// Rate limit
	if v := os.Getenv("CR_RATE_LIMIT_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid CR_RATE_LIMIT_PER_MINUTE: %q (want non-negative integer)", v)
		}
		cfg.RateLimitPerMinute = n
	}

	// Inbound inbox request body limit. This is strict and always enabled: a
	// non-positive or non-integer value is a startup error rather than a silent
	// unlimited-body fallback.
	if v := os.Getenv("CR_INBOX_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_INBOX_MAX_BODY_BYTES: %q (want positive integer)", v)
		}
		cfg.InboxMaxBodyBytes = n
	}

	// Global ingest budget (CR-FEAT-035). Opt-in, and 0 — the default — means
	// the budget does not exist: no delivery is ever shed by it and the
	// delivery path is identical to a build without the feature, which is the
	// contract this row was filed under. A negative value is a startup error
	// rather than a silent "disabled", because "off" already has a spelling.
	if v := os.Getenv("CR_RATE_LIMIT_GLOBAL_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid CR_RATE_LIMIT_GLOBAL_PER_MINUTE: %q (want non-negative integer)", v)
		}
		cfg.GlobalRateLimitPerMinute = n
	}

	// WebSocket allowed origins (comma-separated, "*" = allow all)
	cfg.WSAllowedOrigins = os.Getenv("CR_WS_ALLOWED_ORIGINS")

	// Federation (CR-FEAT-006): relay-to-relay links, comma-separated base
	// URLs. Empty = federation disabled (deliveries to unknown agents 404
	// as before).
	if v := os.Getenv("CR_FED_LINKS"); v != "" {
		cfg.Federation.Links = splitTrim(v)
	}
	cfg.Federation.Name = os.Getenv("CR_FED_NAME")
	// CR_FED_TOKEN (DF-CRIER-6): optional shared secret for outbound link
	// auth, sent as "Authorization: Bearer <token>" to linked relays. Empty
	// (default) = unauthenticated links, as before. Never logged or served.
	cfg.Federation.Token = os.Getenv("CR_FED_TOKEN")
	// CR_FED_MAX_HOLD_S (DF-CRIER-7, spec §8/§9): how long a delivery whose
	// links are all transiently down is held at this relay before the sender
	// gets an explicit FEDERATION_FAILED outcome. Default 300s.
	if v := os.Getenv("CR_FED_MAX_HOLD_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_FED_MAX_HOLD_S: %q (want positive seconds)", v)
		}
		cfg.Federation.MaxHold = time.Duration(n) * time.Second
	}
	// CR_FED_QUEUE_FILE (DF-CRIER-7): path of the durable hold-queue
	// document. Unset (default) keeps held deliveries in process memory
	// only, matching the in-memory registry backend contract.
	cfg.Federation.QueueFile = os.Getenv("CR_FED_QUEUE_FILE")

	// Per-agent request signing enforcement. Default true (secure).
	// Set CR_REQUIRE_AGENT_SIG=false only for trusted single-user setups.
	if v := os.Getenv("CR_REQUIRE_AGENT_SIG"); v != "" {
		parsed, err := parseTolerantBool("CR_REQUIRE_AGENT_SIG", v)
		if err != nil {
			return cfg, err
		}
		cfg.RequireAgentSig = parsed
	}

	// Mesh connect authentication (DF-CRIER-287). Default FALSE, and that is
	// deliberate: the mesh is the only lane whose clients may have no key
	// setup at all, so flipping the default would refuse every existing
	// client at once instead of asking the operator to opt in. When on, a
	// connecting agent must hold the private half of the key the registry
	// holds for the id in the connect URL.
	if v := os.Getenv("CR_REQUIRE_MESH_AUTH"); v != "" {
		parsed, err := parseTolerantBool("CR_REQUIRE_MESH_AUTH", v)
		if err != nil {
			return cfg, err
		}
		cfg.RequireMeshAuth = parsed
	}
	// CR_MESH_AUTH_TIMEOUT_S bounds the challenge/response. Unset leaves the
	// zero value, which the mesh resolves to its own default
	// (mesh.DefaultMeshAuthTimeout) — one constant, not a copy of it here.
	if v := os.Getenv("CR_MESH_AUTH_TIMEOUT_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_MESH_AUTH_TIMEOUT_S: %q (want positive seconds)", v)
		}
		cfg.MeshAuthTimeout = time.Duration(n) * time.Second
	}
	// CR_MESH_ALLOWED_ORIGINS is the MESH-ONLY origin allowlist, separate from
	// CR_WS_ALLOWED_ORIGINS (which also covers the relay). Unset (default)
	// allows every origin, as the mesh always has.
	cfg.MeshAllowedOrigins = os.Getenv("CR_MESH_ALLOWED_ORIGINS")

	// CR_PRESENCE_STALE_AFTER_S is how long a registry row may go without
	// liveness evidence before the status reported for it becomes "stale"
	// (CR-FEAT-024). Unset leaves the zero value, which the registry resolves
	// to its own default (registry.DefaultStalenessWindow) — one constant, not
	// a copy of it here. A non-positive value is refused rather than honored:
	// a window of 0 would report every row stale the instant its last
	// heartbeat was a millisecond old, which is a broken presence signal
	// masquerading as a strict one (the fail-closed direction is wrong here).
	if v := os.Getenv("CR_PRESENCE_STALE_AFTER_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_PRESENCE_STALE_AFTER_S: %q (want positive seconds)", v)
		}
		cfg.PresenceStaleAfter = time.Duration(n) * time.Second
	}

	// CR_IDEMPOTENCY_WINDOW_S is the window within which a sender-supplied
	// idempotency key deduplicates a delivery (CR-FEAT-025). Unset leaves the
	// zero value, which the registry resolves to its own default
	// (registry.DefaultIdempotencyWindow) — one constant, not a copy of it
	// here. A non-positive value is refused rather than honored: a window of 0
	// would silently mean "deduplicate nothing", which is the feature being
	// absent while the deliver schema still documents it.
	if v := os.Getenv("CR_IDEMPOTENCY_WINDOW_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_IDEMPOTENCY_WINDOW_S: %q (want positive seconds)", v)
		}
		cfg.IdempotencyWindow = time.Duration(n) * time.Second
	}

	// Webhook delivery tuning (specs/WEBHOOK-DELIVERY.md §9).
	cfg.Webhook.Secret = os.Getenv("CR_WEBHOOK_SECRET")
	if v := os.Getenv("CR_WEBHOOK_TIMEOUT_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_TIMEOUT_S: %q", v)
		}
		cfg.Webhook.Timeout = time.Duration(n) * time.Second
	}
	if v := os.Getenv("CR_WEBHOOK_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_MAX_RETRIES: %q", v)
		}
		cfg.Webhook.MaxRetries = n
	}
	if v := os.Getenv("CR_WEBHOOK_REDELIVER_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_REDELIVER_S: %q", v)
		}
		cfg.Webhook.RedeliverEvery = time.Duration(n) * time.Second
	}
	if v := os.Getenv("CR_WEBHOOK_PROBE_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_PROBE_S: %q", v)
		}
		cfg.Webhook.ProbeEvery = time.Duration(n) * time.Second
	}
	if v := os.Getenv("CR_WEBHOOK_CIRCUIT_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_CIRCUIT_THRESHOLD: %q", v)
		}
		cfg.Webhook.CircuitThreshold = n
	}
	// Batch flush controls (CR-FEAT-005): per-agent registration values
	// override these defaults at delivery time.
	if v := os.Getenv("CR_WEBHOOK_BATCH_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_BATCH_MAX: %q", v)
		}
		cfg.Webhook.BatchMaxMessages = n
	}
	if v := os.Getenv("CR_WEBHOOK_BATCH_FLUSH_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_WEBHOOK_BATCH_FLUSH_S: %q", v)
		}
		cfg.Webhook.BatchFlushInterval = time.Duration(n) * time.Second
	}

	// LLM message guard (CR-FEAT-010, spec §9.1).
	if v := os.Getenv("CR_GUARD_ENABLED"); v != "" {
		parsed, err := parseTolerantBool("CR_GUARD_ENABLED", v)
		if err != nil {
			return cfg, err
		}
		cfg.Guard.Enabled = parsed
	}
	if v := os.Getenv("CR_GUARD_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_TIMEOUT_MS: %q", v)
		}
		cfg.Guard.Timeout = time.Duration(n) * time.Millisecond
	}
	if v := os.Getenv("CR_GUARD_MAX_CONCURRENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_MAX_CONCURRENT: %q", v)
		}
		cfg.Guard.MaxConcurrent = n
	}
	if v := os.Getenv("CR_GUARD_CIRCUIT_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_CIRCUIT_THRESHOLD: %q", v)
		}
		cfg.Guard.CircuitThreshold = n
	}
	if v := os.Getenv("CR_GUARD_CIRCUIT_COOLDOWN_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_CIRCUIT_COOLDOWN_S: %q", v)
		}
		cfg.Guard.CircuitCooldown = time.Duration(n) * time.Second
	}
	if v := os.Getenv("CR_GUARD_MAX_PAYLOAD_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_MAX_PAYLOAD_BYTES: %q", v)
		}
		cfg.Guard.MaxPayloadBytes = n
	}
	if v := os.Getenv("CR_GUARD_RENDER_MAX_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_RENDER_MAX_BYTES: %q", v)
		}
		cfg.Guard.RenderMaxBytes = n
	}
	if v := os.Getenv("CR_GUARD_KANBAN_QUEUE"); v != "" {
		// CR-FEAT-014 (spec §8.2): kanban worker queue capacity. The
		// worker itself is enabled per-policy via policy.kanban.enabled.
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_GUARD_KANBAN_QUEUE: %q", v)
		}
		cfg.Guard.KanbanQueueSize = n
	}
	// CR_GUARD_KANBAN_URL (CR-FEAT-009): HTTP kanban sink base URL. Empty
	// = the Hermes kanban CLI writer. Scheme validation (http/https) is
	// deferred to writer construction in cmd/server — it fails fast there
	// (spec §9.1) when the value is set but unusable.
	cfg.Guard.KanbanURL = os.Getenv("CR_GUARD_KANBAN_URL")
	if v := os.Getenv("CR_GUARD_DEEPSEEK_BASE_URL"); v != "" {
		cfg.Guard.DeepSeekBaseURL = v
	}
	if v := os.Getenv("CR_GUARD_MODEL"); v != "" {
		cfg.Guard.Model = v
	}
	// CR_GUARD_PATTERNS_EXTRA is passed through; it is parsed and validated
	// by guard.New at startup (a broken pattern table fails fast, spec §9.1).
	cfg.Guard.ExtraPatterns = os.Getenv("CR_GUARD_PATTERNS_EXTRA")
	// CR_GUARD_DEFAULT_POLICY is passed through; parsed + validated by
	// guard.New at startup (a broken server default fails fast, spec §9.1).
	cfg.Guard.DefaultPolicy = os.Getenv("CR_GUARD_DEFAULT_POLICY")

	// Opt-in live-inspection surfaces (DF-CRIER-142). Same tolerant bool
	// dialect as CR_REQUIRE_AGENT_SIG / CR_GUARD_ENABLED; default false, so
	// an unset flag leaves the surface unregistered (404).
	if v := os.Getenv("CR_ENABLE_PPROF"); v != "" {
		parsed, err := parseTolerantBool("CR_ENABLE_PPROF", v)
		if err != nil {
			return cfg, err
		}
		cfg.Observability.EnablePProf = parsed
	}
	if v := os.Getenv("CR_ENABLE_METRICS"); v != "" {
		parsed, err := parseTolerantBool("CR_ENABLE_METRICS", v)
		if err != nil {
			return cfg, err
		}
		cfg.Observability.EnableMetrics = parsed
	}

	// Opt-in A2A interoperability (INT-A2A-001, specs/A2A-OPTION.md §4.1).
	// Same tolerant bool dialect as the other switches; default false, so an
	// unset flag leaves the option OFF and nothing A2A-related registered.
	// The value is carried here only — no surface reads it yet, and the
	// per-agent half of the gate is the optional `a2a` block on a registry
	// row (§4.2).
	if v := os.Getenv("CR_A2A_ENABLED"); v != "" {
		parsed, err := parseTolerantBool("CR_A2A_ENABLED", v)
		if err != nil {
			return cfg, err
		}
		cfg.A2AEnabled = parsed
	}

	// Detection & containment (CR-FEAT-030, specs/DETECTION.md). OPT-IN: with
	// CR_DETECT_ENABLED unset the server registers no detection route, writes
	// no log and behaves exactly as before.
	if v := os.Getenv("CR_DETECT_ENABLED"); v != "" {
		parsed, err := parseTolerantBool("CR_DETECT_ENABLED", v)
		if err != nil {
			return cfg, err
		}
		cfg.Detection.Enabled = parsed
	}
	cfg.Detection.LogPath = os.Getenv("CR_DETECT_LOG")
	cfg.Detection.KeyPath = os.Getenv("CR_DETECT_KEY")
	// The quiet window is the one detection setting whose DEFAULT is a value
	// rather than "whatever internal/detect resolves": the zero pair would be
	// ambiguous between "unset" and "disabled", so the documented window is
	// applied here (from the detect package's own constants) and an explicit
	// CR_DETECT_QUIET_HOURS=0-0 is what turns the signal off.
	cfg.Detection.QuietStartHour = detect.DefaultQuietStartHour
	cfg.Detection.QuietEndHour = detect.DefaultQuietEndHour
	// A configured log without a key path signs with <log>.key: the log is
	// only worth keeping if it can be verified after a restart, and a missing
	// key path must not silently mean "unsigned".
	if cfg.Detection.LogPath != "" && cfg.Detection.KeyPath == "" {
		cfg.Detection.KeyPath = cfg.Detection.LogPath + ".key"
	}
	if v := os.Getenv("CR_DETECT_FANOUT_WINDOW_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DETECT_FANOUT_WINDOW_S: %q (want positive seconds)", v)
		}
		cfg.Detection.FanoutWindow = time.Duration(n) * time.Second
	}
	if v := os.Getenv("CR_DETECT_FANOUT_MIN_TARGETS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DETECT_FANOUT_MIN_TARGETS: %q (want a positive count)", v)
		}
		cfg.Detection.FanoutMinTargets = n
	}
	if v := os.Getenv("CR_DETECT_NEWPEER_WINDOW_S"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DETECT_NEWPEER_WINDOW_S: %q (want positive seconds)", v)
		}
		cfg.Detection.NewPeerWindow = time.Duration(n) * time.Second
	}
	if v := os.Getenv("CR_DETECT_NEWPEER_MIN_TARGETS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DETECT_NEWPEER_MIN_TARGETS: %q (want a positive count)", v)
		}
		cfg.Detection.NewPeerMinTargets = n
	}
	if v := os.Getenv("CR_DETECT_QUIET_HOURS"); v != "" {
		start, end, err := parseHourRange("CR_DETECT_QUIET_HOURS", v)
		if err != nil {
			return cfg, err
		}
		cfg.Detection.QuietStartHour = start
		cfg.Detection.QuietEndHour = end
	}
	if v := os.Getenv("CR_DETECT_QUIET_MIN_MESSAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DETECT_QUIET_MIN_MESSAGES: %q (want a positive count)", v)
		}
		cfg.Detection.QuietMinMessages = n
	}
	if v := os.Getenv("CR_CANARY_TOKENS"); v != "" {
		for _, tok := range splitTrim(v) {
			cfg.Detection.CanaryTokens = append(cfg.Detection.CanaryTokens, tok)
		}
	}

	// Realms (CR-FEAT-029). The document is passed through to
	// internal/namespace, which parses and validates it at startup; the one
	// rule enforced HERE is that the two spellings cannot both be set, because
	// a silent precedence rule is exactly the kind of ambiguity that makes an
	// operator read the source to find out which realm is live.
	cfg.Namespaces.Inline = os.Getenv("CR_NAMESPACES")
	cfg.Namespaces.File = os.Getenv("CR_NAMESPACES_FILE")
	if strings.TrimSpace(cfg.Namespaces.Inline) != "" && strings.TrimSpace(cfg.Namespaces.File) != "" {
		return cfg, fmt.Errorf("CR_NAMESPACES and CR_NAMESPACES_FILE are both set — declare the namespace document in exactly one place")
	}

	return cfg, nil
}

// parseHourRange reads a "S-E" UTC hour range (0..23 each, start inclusive,
// end exclusive). A declared range is HONORED or REFUSED, never ignored: an
// operator who writes "22-6" gets the wrap-around window, and one who writes
// "25-3" gets a startup error naming the bound.
func parseHourRange(name, v string) (int, int, error) {
	parts := strings.SplitN(v, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid %s: %q (want S-E, e.g. 1-5)", name, v)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start < 0 || start > 23 {
		return 0, 0, fmt.Errorf("invalid %s: %q (start hour must be 0..23)", name, v)
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || end < 0 || end > 23 {
		return 0, 0, fmt.Errorf("invalid %s: %q (end hour must be 0..23)", name, v)
	}
	return start, end, nil
}

// parseTolerantBool is the single tolerant boolean dialect crier reads env
// vars with (CR_REQUIRE_AGENT_SIG, CR_GUARD_ENABLED, CR_ENABLE_PPROF,
// CR_ENABLE_METRICS, CR_A2A_ENABLED): true/1/yes and false/0/no,
// case-insensitive, anything else fails loudly naming the variable.
func parseTolerantBool(name, v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid %s: %q (want true/false)", name, v)
	}
}

// BuildCheckOrigin returns a CheckOrigin function for gorilla/websocket.
// If allowed is empty or "*", all origins are permitted.
// Otherwise, allowed is a comma-separated list of origins (scheme://host:port).
// At least one origin in the list must match the request's Origin header.
func BuildCheckOrigin(allowed string) func(r *http.Request) bool {
	if allowed == "" || allowed == "*" {
		return func(r *http.Request) bool { return true }
	}
	allowedSet := make(map[string]bool)
	for _, origin := range splitTrim(allowed) {
		allowedSet[origin] = true
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return allowedSet[origin]
	}
}

// MeshOriginPolicy names the policy a mesh origin allowlist resolves to. It is
// the value GET /status reports ("allow-all" or "allowlist"), so an operator
// can tell a permissive mesh from a restricted one without reading the
// environment — the same reason the federation hold queue reports a mode and
// not a path.
const (
	MeshOriginPolicyAllowAll  = "allow-all"
	MeshOriginPolicyAllowlist = "allowlist"
)

// MeshOriginPolicy returns the mode CR_MESH_ALLOWED_ORIGINS resolves to.
func MeshOriginPolicy(allowed string) string {
	if allowed == "" || allowed == "*" {
		return MeshOriginPolicyAllowAll
	}
	return MeshOriginPolicyAllowlist
}

// BuildMeshCheckOrigin returns the mesh upgrader's CheckOrigin (DF-CRIER-287).
//
// It is deliberately NOT BuildCheckOrigin with a different string. The two
// lanes have different clients: the relay is subscribed to from browsers, so
// its allowlist may reasonably require an Origin; the mesh is connected to by
// agents, and every shipped mesh client (this repo's Go client, the Python
// worked example, the MCP bridge) sends NO Origin header at all. Reusing the
// relay's rule on the mesh would therefore reject every legitimate agent the
// moment an operator set an allowlist — an outage dressed up as a policy.
//
// The rule here is stated in the doc, in the same words:
//
//   - allowed is empty or "*" (the default): every origin — allow-all, the
//     mesh's historical behaviour, unchanged.
//   - otherwise: a request that CARRIES an Origin header must name one of the
//     listed origins; a request that carries NONE is allowed, because a
//     non-browser agent client has no origin to declare and the header is the
//     only thing a policy can bind to. That keeps the protection where it
//     means something — a browser (or anything else that self-identifies with
//     an Origin) can no longer open a mesh socket from an unlisted page —
//     without locking out the agent clients the lane exists for.
//
// A rejected upgrade is a 403 from gorilla/websocket's Upgrade, before the
// handshake and before any frame.
func BuildMeshCheckOrigin(allowed string) func(r *http.Request) bool {
	if allowed == "" || allowed == "*" {
		return func(r *http.Request) bool { return true }
	}
	allowedSet := make(map[string]bool)
	for _, origin := range splitTrim(allowed) {
		allowedSet[origin] = true
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return allowedSet[origin]
	}
}

// splitTrim splits a comma-separated string and trims whitespace from each element.
func splitTrim(s string) []string {
	parts := make([]string, 0)
	for _, part := range splitComma(s) {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

func splitComma(s string) []string {
	return strings.Split(s, ",")
}
