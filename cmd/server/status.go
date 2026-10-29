package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/registry"
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
// below historically reported an effective BOOLEAN or MODE, never a value: no CR_AUTH_TOKEN, no
// CR_DATABASE_URL (not even redacted — the backend name is the operational
// fact; the connection string, including any password in it, is not), no
// CR_WEBHOOK_SECRET, no CR_FED_TOKEN, no guard provider credentials and no
// guard base URL. A hostname or a filesystem path can leak topology, so the
// federation hold queue reports its durability MODE ("none"/"memory"/"file")
// rather than CR_FED_QUEUE_FILE's path. DF-CRIER-413 intentionally adds the
// resolved local session path so operators can verify the storage target; it
// exposes no database URL, credential, mount source or host-side volume path.
//
// CR-FEAT-035 adds two fields that are not posture at all, and the distinction
// is deliberate rather than a lapse:
//
//   - "global_rate_limit_per_minute" is posture (the effective value of the
//     CR_RATE_LIMIT_GLOBAL_PER_MINUTE budget, 0 = none), the same class as the
//     per-agent "rate_limit_per_minute" it sits beside;
//   - "queue_depth" is a MEASUREMENT — how much is queued right now — which the
//     endpoint reports because "nothing sheds load and nothing reports depth"
//     is exactly the blind spot the review named: an operator holding /health
//     "ok" could not tell a healthy relay from one 40 000 messages behind. It
//     carries counts and an age, never a payload, a message id, an agent id or
//     any configuration, and it is read per request (a snapshot, not a cache).
//     A store that cannot report (a remote proxy) reports null rather than a
//     fabricated zero.
//
// CR-CHAT-020 adds the read surfaces the chat UI draws and the bus could not
// justify — each one a DERIVATION over signals the bus already holds, never a
// parallel truth (CHAT-INTERFACE.md §5.1):
//
//   - "queue_depth_history" — a timestamped ring of recent queue measurements
//     (the sparkline the UI draws). It is sampled on each /status read; the
//     ring is process-lifetime, and null (not an empty array) when the serving
//     store cannot report a depth, so "no history yet" and "unavailable" are
//     never the same reading.
//   - "health" — a three-word grade derived from the ONE real condition the
//     bus can measure: whether the serving registry backend answers. "ok" is
//     the process up and the backend answering; "degraded" is the process up
//     but the backend not answering; "down" is reserved for the process not
//     answering at all — which is exactly what GET /health reports, so the
//     grade never claims a condition (federation peer loss, guard errors) the
//     bus does not measure as such.
//   - "agents_online" / "agents_total" — the fleet presence aggregate,
//     counted with the SAME Presence derivation GET /agents reports with, so
//     the "N/M agents online" figure cannot disagree with the roster it
//     summarizes.
//
// Auth: /status is NOT on the exempt list in internal/middleware/auth.go, so
// with CR_AUTH_TOKEN set it requires the Bearer header like every other
// authenticated route; with auth disabled it is open. That asymmetry is
// deliberate — the endpoint reports whether auth is enforced, so it must not
// be readable by an unauthenticated caller on a server that HAS auth on. The
// public half of that question ("is this thing up, and which build?") is what
// /health and /version already answer.

// queueDepthReader reads the serving store's live queue depth. ok=false means
// "this store cannot report" (or the read failed), which the surfaces render as
// an absence — null in GET /status, NaN on the metric — rather than as a zero.
type queueDepthReader func() (registry.QueueDepth, bool)

// newQueueDepthReader adapts a store to the reader above. A store that does not
// implement registry.DepthReporter (the remote proxy) yields nil: no reader at
// all, so nothing anywhere reports a depth it cannot measure.
func newQueueDepthReader(store registry.Store) queueDepthReader {
	reporter, ok := store.(registry.DepthReporter)
	if !ok {
		return nil
	}
	return func() (registry.QueueDepth, bool) {
		depth, err := reporter.QueueDepth()
		if err != nil {
			slog.Warn("queue depth unavailable", "error", err)
			return registry.QueueDepth{}, false
		}
		return depth, true
	}
}

// Registry backend names as they appear in the GET /status body. They are the
// schema's enum values, so a rename is a wire change.
const (
	registryBackendMemory   = "memory"
	registryBackendPostgres = "postgres"
)

// Health grades as they appear in the GET /status body (CR-CHAT-020). The
// enum is the schema's, so a rename is a wire change.
const (
	statusHealthOK       = "ok"
	statusHealthDegraded = "degraded"
	statusHealthDown     = "down"
)

// registryBackendForURL preserves the legacy URL-only rule for callers/tests
// without an explicit backend. Production selection uses Config's resolver.
func registryBackendForURL(dbURL string) string {
	return (config.Config{Database: config.DatabaseConfig{URL: dbURL}}).ResolvedRegistryBackend()
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

// statusQueueDepth is the WIRE shape of the live queue measurement (CR-FEAT-035):
// counts plus an age in whole seconds. `oldest_age_s` is 0 when nothing is
// queued — there is no message whose age could be reported — and the object is
// null as a whole when the serving store cannot answer, so "empty" and
// "unavailable" are never the same reading.
type statusQueueDepth struct {
	// Pending is the number of un-acknowledged, un-expired messages across all
	// inboxes (leased messages included: a leased message is still queued).
	Pending int `json:"pending"`
	// Leased is how many of Pending are currently held under a live lease.
	Leased int `json:"leased"`
	// OldestAgeS is how long the oldest pending message has been waiting.
	OldestAgeS int `json:"oldest_age_s"`
}

// statusQueueSample is one entry of the queue-depth history ring (CR-CHAT-020):
// a measurement and the instant it was taken. The counts are the same shape
// queue_depth carries; the trend glyph the UI draws is a client projection of
// the sequence, not a server-computed slope.
type statusQueueSample struct {
	At         time.Time `json:"at"`
	Pending    int       `json:"pending"`
	Leased     int       `json:"leased"`
	OldestAgeS int       `json:"oldest_age_s"`
}

// queueHistoryCapacity is how many samples the ring keeps (CR-CHAT-020). A
// status read that samples more often than an operator can draw simply wraps
// the ring; capacity bounds memory, not the client's window.
const queueHistoryCapacity = 60

// queueHistory is the process-lifetime ring of recent queue measurements
// (CR-CHAT-020), sampled on each GET /status. It is a measurement sink, not a
// cache: the same request still reports the LIVE queue_depth, and the history
// is the trail of prior snapshots. Nil-capable: the zero value is usable and
// reports an empty history, so a server wired without a depth reader degrades
// honestly.
type queueHistory struct {
	mu      sync.Mutex
	samples []statusQueueSample
}

// record appends one snapshot, dropping the oldest beyond capacity.
func (h *queueHistory) record(at time.Time, depth registry.QueueDepth) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, statusQueueSample{
		At:         at.UTC(),
		Pending:    depth.Pending,
		Leased:     depth.Leased,
		OldestAgeS: int(depth.OldestAge.Seconds()),
	})
	if len(h.samples) > queueHistoryCapacity {
		h.samples = h.samples[len(h.samples)-queueHistoryCapacity:]
	}
}

// snapshot returns the samples oldest-first, or nil when none were taken (a
// server with no depth reader has no history to draw — the field stays null
// rather than lying with an empty array).
func (h *queueHistory) snapshot() []statusQueueSample {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == 0 {
		return nil
	}
	out := make([]statusQueueSample, len(h.samples))
	copy(out, h.samples)
	return out
}

// statusHealth grades deriveHealth from the store probe: the process answering
// this request is up by construction (it is answering), so "ok" and
// "degraded" are the reachable grades here, and "down" exists only so the
// enum is the one GET /health would justify if this endpoint were ever
// unreachable — a word the process cannot honestly say about itself.
func deriveHealth(storeHealthy, probeKnown bool) string {
	if !probeKnown {
		// No backend that can be probed (a remote proxy, a test double): the
		// process is up and nothing is known to be wrong, so the honest word
		// is "ok" — not an invented "unknown" the schema does not carry.
		return statusHealthOK
	}
	if storeHealthy {
		return statusHealthOK
	}
	return statusHealthDegraded
}

// healthProbe is the seam the graded health word reads through: a boolean
// answer plus whether the answer is KNOWN (false, false when no HealthReporter
// is wired — the documented degradation to "ok"). The real probe is the
// serving store's HealthReporter capability.
type healthProbe func() (healthy, known bool)

// statusQueueDepthFrom renders a live measurement as the wire object, or nil
// when the store cannot report. Kept as the value-level seam the CR-FEAT-035
// tests pin; the handler itself goes through statusQueueDepthValue so the
// ok-channel drives the history sampling too.
func statusQueueDepthFrom(read queueDepthReader) *statusQueueDepth {
	d, _ := statusQueueDepthValue(read)
	return d
}

// statusAggregator is the seam the presence aggregate is counted through: the
// real implementation is the registry Handler, which owns the roster and the
// Presence rule; tests stand in a fixed roster. Counted PER REQUEST against
// ONE instant, exactly like GET /agents.
type statusAggregator interface {
	CountAgentsForStatus(now time.Time) registry.AgentsOnlineCount
}

// statusResponse is the wire contract of GET /status. Fields distinguish
// configuration, effective gate state and live counts — see the file comment
// for what is deliberately absent. The Build object is buildinfo.Info itself,
// which is the same type GET /version serializes, so the two surfaces cannot
// disagree about which
// build is answering (DF-CRIER-101/127); it is nested rather than flattened so
// the build identity stays one object on both endpoints.
type statusResponse struct {
	ServiceDeliveryAuthEnabled             bool `json:"service_delivery_auth_enabled"`
	ServiceDeliveryAudiencePresent         bool `json:"service_delivery_audience_present"`
	ServiceDeliveryProtectedRecipients     int  `json:"service_delivery_protected_recipient_count"`
	ServiceDeliveryDirectedPairs           int  `json:"service_delivery_directed_pair_count"`
	ServiceDeliveryDurableIdempotencyReady bool `json:"service_delivery_durable_idempotency_ready"`
	// AuthEnabled reports whether a Bearer token is enforced (CR_AUTH_TOKEN
	// non-empty). False is the documented development posture.
	AuthEnabled bool `json:"auth_enabled"`
	// ManagementSurface distinguishes the disabled ACL from a live ACL whose
	// management gate refuses this viewer (enabled-unarmed). admin-token-gated
	// means this request passes the existing admin gate, not just that a token
	// is configured. No credential or token-derived value is exposed.
	ManagementSurface string `json:"management_surface"`
	// RequireAgentSignature reports whether per-agent ed25519 signing is
	// enforced on agent-scoped routes (CR_REQUIRE_AGENT_SIG, default true).
	RequireAgentSignature bool `json:"require_agent_signature"`
	// MeshAuthRequired reports whether the mesh connect handshake is enforced
	// (CR_REQUIRE_MESH_AUTH, default false): true means a peer must answer an
	// ed25519 challenge before its frames are admitted and before it appears
	// in GET /mesh/peers. False is the documented default, and it is a real
	// posture difference — an operator auditing a live server needs to see it
	// rather than infer it from a log line written at boot.
	MeshAuthRequired bool `json:"mesh_auth_required"`
	// MeshOriginPolicy is "allow-all" or "allowlist" — the mode
	// CR_MESH_ALLOWED_ORIGINS resolves to. The origins themselves are config
	// (hostnames), so only the mode is reported, exactly as the federation
	// hold queue reports its durability mode instead of its path.
	MeshOriginPolicy string `json:"mesh_origin_policy"`
	// GuardEnabled reports the LLM message guard master switch
	// (CR_GUARD_ENABLED, default true).
	GuardEnabled bool `json:"guard_enabled"`
	// PresenceStaleAfterS is the EFFECTIVE staleness window the registry
	// derives a row's status with, in seconds (CR_PRESENCE_STALE_AFTER_S,
	// CR-FEAT-024). It is reported because it is the number a `stale` row was
	// judged by: without it an operator reading GET /agents cannot tell a
	// deliberate 30s window from a misconfigured 10-minute one, and the
	// environment variable that set it lives in a different place than the
	// reading.
	PresenceStaleAfterS int `json:"presence_stale_after_s"`
	// RegistryBackend is "memory", "sqlite" or "postgres" — the backend actually
	// serving this process, derived from the same input that selected it.
	RegistryBackend string `json:"registry_backend"`
	// RegistryDurable distinguishes answering from surviving a restart. Nil
	// means the selected backend has no known persistence contract.
	RegistryDurable *bool `json:"registry_durable"`
	// SessionBackend is resolved by the same Config method used to open it.
	SessionBackend string `json:"session_backend"`
	// SessionStorePath intentionally exposes the resolved local session file
	// or log directory so operators can verify the actual persistence target.
	// Nil for external PostgreSQL or an unresolvable location; never a DSN.
	SessionStorePath *string `json:"session_store_path"`
	// Mount evidence, not a claim that a volume is retained or backed up.
	SessionStoreMount string `json:"session_store_mount"`
	// SessionStoreIgnored names the configuration key an operator set that the
	// ACTIVE session backend does not open, e.g. "ignored:
	// CR_SESSION_SQLITE_PATH (backend is jsonl)". Empty when nothing is
	// ignored. It is printed beside session_store_path because a path the
	// process never opens must never read as the live store (DF-CRIER-428).
	SessionStoreIgnored string `json:"session_store_ignored"`
	// RegistryStorePath is the resolved absolute SQLite registry file actually
	// in use. Null for the in-memory store (no file exists) and for external
	// PostgreSQL (its URL is configuration, never posture).
	RegistryStorePath *string `json:"registry_store_path"`
	// RegistryStoreIgnored is SessionStoreIgnored for the registry: the memory
	// and postgres backends never open CR_REGISTRY_SQLITE_PATH, so a path set
	// alongside them is named as ignored instead of implied durable.
	RegistryStoreIgnored string `json:"registry_store_ignored"`
	// A2AEnabled is the effective route-registration switch, not agent support.
	A2AEnabled bool `json:"a2a_enabled"`
	// RateLimitPerMinute is the effective relay publish budget; 0 means
	// unlimited (CR_RATE_LIMIT_PER_MINUTE).
	RateLimitPerMinute int `json:"rate_limit_per_minute"`
	// GlobalRateLimitPerMinute is the effective global inbox-ingest budget in
	// deliveries per minute across every agent (CR_RATE_LIMIT_GLOBAL_PER_MINUTE,
	// CR-FEAT-035); 0 — the default — means no global budget exists and the
	// delivery path never sheds. It is reported next to the per-agent publish
	// cap because the two are different lanes and an operator deploying one
	// while reading the other must be able to tell them apart.
	GlobalRateLimitPerMinute int `json:"global_rate_limit_per_minute"`
	// QueueDepth is the live store-wide inbox queue: how many messages are
	// waiting, how many of those are held under a lease, and how old the oldest
	// one is. Null when the serving store cannot report it (a remote proxy).
	// It is a measurement, not posture — see the file comment.
	QueueDepth *statusQueueDepth `json:"queue_depth"`
	// QueueDepthHistory is the recent trail of queue_depth snapshots
	// (CR-CHAT-020), oldest-first, at most queueHistoryCapacity samples. Null
	// until at least one sample exists, so "no history yet" is not the same
	// reading as "measured, flat". The trend glyph the UI draws is a client
	// projection of this sequence.
	QueueDepthHistory []statusQueueSample `json:"queue_depth_history"`
	// Health is the graded severity word (CR-CHAT-020): "ok" while the
	// serving registry backend answers its probe, "degraded" when it does
	// not, "down" reserved for the process itself failing to answer (GET
	// /health's word). Never a grade the bus cannot derive.
	Health string `json:"health"`
	// AgentsOnline / AgentsTotal is the fleet presence aggregate
	// (CR-CHAT-020), counted with the same Presence rule GET /agents derives
	// every row's status with. THE count the "N/M agents online" figure
	// renders — no client-side recount.
	AgentsOnline int `json:"agents_online"`
	AgentsTotal  int `json:"agents_total"`
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
	// FederationRequirePeer is the legacy CONFIGURATION alias, retained for
	// compatibility. The explicit pair distinguishes intent from the live gate.
	FederationRequirePeer           bool `json:"federation_require_peer"`
	FederationRequirePeerConfigured bool `json:"federation_require_peer_configured"`
	FederationRequirePeerEffective  bool `json:"federation_require_peer_effective"`
	// Count all lifecycle records, not discovery URLs or only linked peers.
	FederationPeerCount int `json:"federation_peer_count"`
	// Acceptance by the federation gate; deployment auth/policy may still refuse.
	FederationTokenOnlyAccepted bool   `json:"federation_token_only_accepted"`
	FederationEnabled           bool   `json:"federation_enabled"`
	FederationHoldQueue         string `json:"federation_hold_queue"`
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
	policy, _ := registry.ParseServiceDeliveryAllowlist(cfg.ServiceDeliveryAllowlist)
	pairs := 0
	for _, senders := range policy {
		pairs += len(senders)
	}
	var durable *bool
	switch registryBackend {
	case registryBackendMemory:
		value := false
		durable = &value
	case registryBackendPostgres, "sqlite":
		// Registry SQLite only accepts file paths, never :memory: or URIs.
		value := true
		durable = &value
	}
	managementSurface := "disabled"
	if cfg.Permissions.Enabled {
		// A configuration-only builder cannot vouch for a viewer's access.
		managementSurface = "enabled-unarmed"
	}
	sessionPath, sessionMount := sessionStoreLocation(cfg)
	registryPath := registryStoreLocation(cfg)
	return statusResponse{
		SessionStorePath:                       sessionPath,
		SessionStoreMount:                      sessionMount,
		SessionStoreIgnored:                    cfg.SessionSQLitePathIgnored(),
		RegistryStorePath:                      registryPath,
		RegistryStoreIgnored:                   cfg.RegistrySQLitePathIgnored(),
		ManagementSurface:                      managementSurface,
		RegistryDurable:                        durable,
		SessionBackend:                         cfg.ResolvedSessionBackend(),
		A2AEnabled:                             cfg.A2AEnabled,
		ServiceDeliveryAuthEnabled:             len(policy) > 0,
		ServiceDeliveryAudiencePresent:         cfg.ServiceDeliveryAudience != "",
		ServiceDeliveryProtectedRecipients:     len(policy),
		ServiceDeliveryDirectedPairs:           pairs,
		ServiceDeliveryDurableIdempotencyReady: len(policy) == 0 || registryBackend == registryBackendPostgres || registryBackend == "sqlite",
		AuthEnabled:                            cfg.AuthToken != "",
		RequireAgentSignature:                  cfg.RequireAgentSig,
		MeshAuthRequired:                       cfg.RequireMeshAuth,
		MeshOriginPolicy:                       config.MeshOriginPolicy(cfg.MeshAllowedOrigins),
		GuardEnabled:                           cfg.Guard.Enabled,
		// The EFFECTIVE window, resolved exactly as the registry handler
		// resolves it (registry.Presence.StaleAfter) — so an unset or
		// malformed-shaped setting reports the window actually in force rather
		// than a zero the derivation never uses (CR-FEAT-024).
		PresenceStaleAfterS:             int(registry.NewPresence(cfg.PresenceStaleAfter).StaleAfter().Seconds()),
		RegistryBackend:                 registryBackend,
		RateLimitPerMinute:              cfg.RateLimitPerMinute,
		LogLevel:                        cfg.LogLevel,
		LogFormat:                       cfg.LogFormat,
		WebhookSigning:                  cfg.Webhook.Secret != "",
		FederationRequirePeer:           cfg.Federation.RequirePeer,
		FederationRequirePeerConfigured: cfg.Federation.RequirePeer,
		// No live gate is wired to this pure builder: never infer effect
		// from config, link URLs or a file path. The handler samples it below.
		FederationTokenOnlyAccepted: true,
		FederationEnabled:           len(cfg.Federation.Links) > 0,
		FederationHoldQueue:         federationHoldQueueMode(cfg),
		// The global ingest budget (CR-FEAT-035). Read from the same
		// configuration the handler was installed from, so a server that had
		// no budget installed cannot advertise one.
		GlobalRateLimitPerMinute: cfg.GlobalRateLimitPerMinute,
		MetricsEnabled:           cfg.Observability.EnableMetrics,
		PProfEnabled:             cfg.Observability.EnablePProf,
		Build:                    buildinfo.Resolve(),
	}
}

// newStatusHandler returns the GET /status handler for a booted server. The
// configuration and the selected backend are captured at boot — they cannot
// change while the process serves — so the handler reads no globals and stays
// testable without env plumbing (the same seam registerObservability uses).
//
// It is the no-store form: the live queue measurement is reported as null
// because there is no store to read it from. run() mounts newStatusHandlerWithQueue
// instead, so the served endpoint reports the real thing.
func newStatusHandler(cfg config.Config, registryBackend string) http.HandlerFunc {
	return newStatusHandlerWithQueue(cfg, registryBackend, nil)
}

// newStatusHandlerWithQueue is newStatusHandler plus the live inbox queue
// measurement (CR-FEAT-035). read is consulted PER REQUEST — the whole point of
// the field is that it changes while the posture does not — and a nil reader
// (or a store that cannot report) serializes as `"queue_depth": null`.
//
// CR-CHAT-020: the same read also (a) feeds the queue-depth history ring, so
// the sparkline the UI draws accumulates from the live measurement rather
// than a second timer racing it, (b) derives the graded health word from the
// store's own probe, and (c) counts the fleet presence aggregate from the
// same roster GET /agents serves, so the figure cannot be computed two ways.
//
// Encoding a fixed-shape struct of booleans, small ints, short strings, a small
// nested measurement and a nested identity cannot fail; a write error here
// means the client went away, which the connection layer already reports (see
// handleVersion).
func newStatusHandlerWithQueue(cfg config.Config, registryBackend string, read queueDepthReader) http.HandlerFunc {
	return newStatusHandlerFull(cfg, registryBackend, read, newQueueHistory(), nil, nil)
}

// newQueueHistory returns a fresh history ring (CR-CHAT-020). One ring per
// mounted endpoint; the server mounts exactly one /status, so one ring serves
// every reader of the trail.
func newQueueHistory() *queueHistory { return &queueHistory{} }

// newStatusHandlerFull is the complete wiring behind both forms above: the
// queue-depth reader, the history ring (shared by every request that samples
// it), the health probe and the presence-aggregate source. The nil-able
// pieces are the documented degradations: no reader → null queue_depth and
// null history; no probe → "ok" (nothing known to be wrong); no aggregator →
// agents_online 0 / agents_total 0 (a server that mounts no registry Handler
// has no fleet to count — the zero figure is the true one, never a guess).
func newStatusHandlerFull(cfg config.Config, registryBackend string, read queueDepthReader, history *queueHistory, probe healthProbe, aggregator statusAggregator, federationPosture ...func() federation.InboundPosture) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := buildStatusResponse(cfg, registryBackend)
		if cfg.Permissions.Enabled {
			// Production passes the live registry handler as the aggregator.
			// Reuse its actual gate; an unwired handler stays fail-closed.
			if gate, ok := aggregator.(interface {
				PermissionsManagementAuthorized(*http.Request) bool
			}); ok && gate.PermissionsManagementAuthorized(r) {
				resp.ManagementSurface = "admin-token-gated"
			}
		}
		// This field is viewer-specific; never share an authorized snapshot.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		if len(federationPosture) > 0 && federationPosture[0] != nil {
			p := federationPosture[0]()
			resp.FederationRequirePeerConfigured = p.RequirePeerConfigured
			resp.FederationRequirePeerEffective = p.RequirePeerEffective
			resp.FederationPeerCount = p.Peers
			resp.FederationTokenOnlyAccepted = p.TokenOnlyAccepted
		}
		if depth, ok := statusQueueDepthValue(read); ok {
			resp.QueueDepth = depth
			// Sample the history from the SAME measurement this response
			// reports — the sparkline can never disagree with the figure it
			// trails.
			history.record(time.Now(), registry.QueueDepth{
				Pending:   depth.Pending,
				Leased:    depth.Leased,
				OldestAge: time.Duration(depth.OldestAgeS) * time.Second,
			})
			resp.QueueDepthHistory = history.snapshot()
		}
		if probe != nil {
			healthy, known := probe()
			resp.Health = deriveHealth(healthy, known)
		} else {
			resp.Health = deriveHealth(false, false)
		}
		// The fleet presence aggregate: counted from the same derivation
		// GET /agents reports with (one instant, one rule), so "N/M online"
		// cannot disagree with the roster it summarizes.
		if aggregator != nil {
			counts := aggregator.CountAgentsForStatus(time.Now())
			resp.AgentsOnline = counts.Online
			resp.AgentsTotal = counts.Total
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// ---------- GET /status/completed (DF-CRIER-335) ----------
//
// The control panel's "completion" state was ambiguous: GET /status reports the
// live queue measurement, but a client reading an empty inbox batch (an empty
// `messages` array) cannot tell "the relay has nothing left to deliver" from
// "nothing is queued for ME", and every operator wanting the wider answer had to
// conjoin `pending` and `leased` by hand, per read. This endpoint states the
// definition ONCE, on the server, and reports the facts it is derived from, so
// no caller re-derives it differently:
//
//	completed          = inbox_empty AND no_pending_leases
//	inbox_empty        = (pending == 0)   // nothing unacknowledged, unexpired
//	no_pending_leases  = (leased == 0)    // no message held under a live lease
//
// "Complete" here is a statement about the RELAY'S WORK QUEUE — nothing left to
// deliver — not an A2A task state. `TaskStateCompleted` is a different
// vocabulary with different evidence, and it is reported on the task surfaces;
// this word is never a proxy for it.
//
// An UNMEASURED queue is never complete. When the serving store cannot report a
// depth (a remote proxy, or a failed read) both conditions are false,
// `queue_depth` is null and `reason` names the absence — the same reading
// GET /status gives by serializing `queue_depth` as null rather than 0. A store
// that cannot answer must not be able to report "done".
//
// Auth: like GET /status, this path is NOT on the auth-exempt list, so with
// CR_AUTH_TOKEN set it requires the Bearer header. It reports the shape of the
// work queue, which is operational data, not the public health question.

// Completion reasons as they appear in the wire body. They are stable strings a
// caller may branch on, so a rename is a wire change.
const (
	// completionReasonUnmeasured: no depth could be read at all. Never combined
	// with completed=true — an unmeasured queue is not an empty one.
	completionReasonUnmeasured = "queue depth unavailable"
	// completionReasonInboxPending: at least one message is unacknowledged and
	// unexpired, so there is work left to deliver.
	completionReasonInboxPending = "inbox not empty"
	// completionReasonLeasesOutstanding: at least one message is held under a
	// live lease. A leased message is still queued (Pending counts it), so this
	// reason stands alone only if a backend ever reports leases without queued
	// messages — reported honestly rather than folded away.
	completionReasonLeasesOutstanding = "leases outstanding"
	// completionReasonPendingAndLeased: both conditions fail.
	completionReasonPendingAndLeased = "inbox not empty and leases outstanding"
)

// statusCompletion is the wire contract of GET /status/completed. The two
// condition booleans are published BESIDE the single `completed` verdict so a
// caller can explain a false without a second request, and `queue_depth` is the
// same object (same shape, same units) GET /status reports, so the two surfaces
// cannot disagree about the numbers they are both derived from.
type statusCompletion struct {
	// Completed is true only when both conditions hold. It is never true for an
	// unmeasured queue.
	Completed bool `json:"completed"`
	// InboxEmpty is the first condition: pending == 0.
	InboxEmpty bool `json:"inbox_empty"`
	// NoPendingLeases is the second condition: leased == 0.
	NoPendingLeases bool `json:"no_pending_leases"`
	// QueueDepth is the live measurement the verdict was derived from — null
	// when the serving store cannot report, exactly as in GET /status. Null is
	// what makes InboxEmpty/NoPendingLeases/Completed false together.
	QueueDepth *statusQueueDepth `json:"queue_depth"`
	// Reason names why Completed is false; empty (omitted) when it is true.
	Reason string `json:"reason,omitempty"`
}

// deriveCompletion states the definition above as code. It takes the ALREADY
// RENDERED measurement (statusQueueDepthValue's output) rather than the raw
// registry reading, so both endpoints render the same numbers one way and a nil
// measurement has exactly one meaning: unmeasured.
func deriveCompletion(q *statusQueueDepth) statusCompletion {
	c := statusCompletion{QueueDepth: q}
	if q == nil {
		c.Reason = completionReasonUnmeasured
		return c
	}
	c.InboxEmpty = q.Pending == 0
	c.NoPendingLeases = q.Leased == 0
	c.Completed = c.InboxEmpty && c.NoPendingLeases
	switch {
	case !c.InboxEmpty && !c.NoPendingLeases:
		c.Reason = completionReasonPendingAndLeased
	case !c.InboxEmpty:
		c.Reason = completionReasonInboxPending
	case !c.NoPendingLeases:
		c.Reason = completionReasonLeasesOutstanding
	}
	return c
}

// newStatusCompletedHandler returns the GET /status/completed handler. read is
// consulted PER REQUEST from the same reader GET /status samples, so a caller
// cannot read a cached verdict, and an unwired reader (nil) reports the
// documented unmeasured answer instead of a fabricated completion.
func newStatusCompletedHandler(read queueDepthReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		depth, _ := statusQueueDepthValue(read)
		resp := deriveCompletion(depth)
		// A live verdict: the answer changes as the queue drains.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// completionPollInterval is how often the background observer samples the
// completion state. It is a constant, not a knob: the loop logs TRANSITIONS, so
// its whole cost is one store read per interval, and an unset-by-everyone
// configuration key would be worse than a documented number.
const completionPollInterval = 30 * time.Second

// completionWatcher samples the completion state and logs each TRANSITION —
// entering "complete" and leaving it. It is the loop, not a second source of
// truth: it reads through the same queueDepthReader GET /status/completed
// serves, and it keeps nothing but the previous verdict, so it cannot disagree
// with an HTTP read taken at the same instant.
//
// The first sample logs only when it is already complete (an operator wants to
// know "nothing is queued" as soon as the process can say it); repeating an
// unchanged verdict logs nothing, so a drained relay is one line, not one line
// every interval.
type completionWatcher struct {
	read     queueDepthReader
	log      *slog.Logger
	interval time.Duration
	last     *bool
}

// newCompletionWatcher returns an unwired-until-run watcher. A nil reader is
// allowed: the loop then observes "unmeasured" every interval, which is never a
// completion transition, so a server without a depth-capable store stays silent
// rather than logging a state it could not measure.
func newCompletionWatcher(read queueDepthReader, log *slog.Logger) *completionWatcher {
	if log == nil {
		log = slog.Default()
	}
	return &completionWatcher{read: read, log: log, interval: completionPollInterval}
}

// sample takes one observation and logs it when the verdict CHANGED (or when the
// first observation is already complete). The returned token names what was
// logged — "complete", "incomplete" or "" for none — so tests assert on the
// transition logic instead of parsing log text.
func (w *completionWatcher) sample() (statusCompletion, string) {
	depth, _ := statusQueueDepthValue(w.read)
	c := deriveCompletion(depth)
	event := ""
	switch {
	case w.last == nil:
		// First observation: only a complete queue is worth a line; an
		// incomplete one is the normal state and would be noise.
		if c.Completed {
			event = "complete"
		}
	case !*w.last && c.Completed:
		event = "complete"
	case *w.last && !c.Completed:
		event = "incomplete"
	}
	verdict := c.Completed
	w.last = &verdict
	switch event {
	case "complete":
		w.log.Info("work queue complete — inbox empty and no pending leases",
			"pending", pendingOf(c), "leased", leasedOf(c))
	case "incomplete":
		w.log.Info("work queue no longer complete",
			"pending", pendingOf(c), "leased", leasedOf(c), "reason", c.Reason)
	}
	return c, event
}

// pendingOf / leasedOf read the counts off the verdict's own measurement, so a
// log line cannot quote a number the served body does not carry.
func pendingOf(c statusCompletion) int {
	if c.QueueDepth == nil {
		return 0
	}
	return c.QueueDepth.Pending
}

func leasedOf(c statusCompletion) int {
	if c.QueueDepth == nil {
		return 0
	}
	return c.QueueDepth.Leased
}

// run samples every interval until ctx is cancelled — the graceful-shutdown
// path cancels it beside the expired-message purge loop, so the watcher never
// outlives the server it observes.
func (w *completionWatcher) run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = completionPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sample()
		}
	}
}

// statusQueueDepthValue renders a live measurement as the wire object; ok is
// false when there is no reader or the store cannot report.
func statusQueueDepthValue(read queueDepthReader) (*statusQueueDepth, bool) {
	if read == nil {
		return nil, false
	}
	depth, ok := read()
	if !ok {
		return nil, false
	}
	return &statusQueueDepth{
		Pending:    depth.Pending,
		Leased:     depth.Leased,
		OldestAgeS: int(depth.OldestAge.Seconds()),
	}, true
}
