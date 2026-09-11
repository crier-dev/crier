package config

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
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
	WSAllowedOrigins   string
	// RequireAgentSig enforces per-agent ed25519 request signing on inbox
	// read/ack/stats and agent deletion. Secure by default.
	RequireAgentSig bool
	// Webhook holds push-delivery tuning (specs/WEBHOOK-DELIVERY.md §9).
	Webhook WebhookConfig
	// Federation holds relay-to-relay link configuration (CR-FEAT-006).
	Federation FederationConfig
	// Guard holds LLM message-guard tuning (specs/LLM-MESSAGE-GUARD.md
	// §9.1, CR-FEAT-010).
	Guard GuardConfig
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

	// Per-agent request signing enforcement. Default true (secure).
	// Set CR_REQUIRE_AGENT_SIG=false only for trusted single-user setups.
	if v := os.Getenv("CR_REQUIRE_AGENT_SIG"); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			cfg.RequireAgentSig = true
		case "false", "0", "no":
			cfg.RequireAgentSig = false
		default:
			return cfg, fmt.Errorf("invalid CR_REQUIRE_AGENT_SIG: %q (want true/false)", v)
		}
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
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			cfg.Guard.Enabled = true
		case "false", "0", "no":
			cfg.Guard.Enabled = false
		default:
			return cfg, fmt.Errorf("invalid CR_GUARD_ENABLED: %q (want true/false)", v)
		}
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

	return cfg, nil
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
