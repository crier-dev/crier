package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/detect"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/pidfile"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/gorilla/mux"
)

// The build identity printed by -version and served by GET /version lives in
// internal/buildinfo — one source for both binaries (DF-CRIER-127). The
// identity is stamped at link time by the Makefile
// (-X .../internal/buildinfo.Version=...) and otherwise falls back to the
// vcs.revision/vcs.time/vcs.modified metadata the Go toolchain records in
// every main package built inside a git checkout.

func main() {
	os.Exit(run(os.Args[1:]))
}

// The process-wide signal guard (CI-018 + QA-CRIER-17).
//
// QA-CRIER-17 arms the per-run SIGINT/SIGTERM handler at the TOP of run()
// precisely so that a caller which signals the process after an early return
// does not die from the default action. CI-018 stops that per-run registration
// when its run() returns — a returned run() must not keep consuming later
// process signals — and stopping the LAST registration would restore the
// default action, i.e. reopen the exact window QA-CRIER-17 closed. This one
// registration, armed once for the life of the process and never stopped,
// keeps the default action disabled no matter how many run() calls have come
// and gone.
//
// It is never read, and that is deliberate: a signal buffered here (size 1, so
// the runtime never blocks on a delivery) is meant to be DISCARDED — the guard
// exists to keep the process alive, not to trigger a shutdown; every live
// server listens on its own per-run channel instead. Measured both ways: with
// this guard armed TestSigtermAfterEarlyBootFailureIsNotFatal is green, and
// without it that fixture's child dies with "signal: terminated".
var (
	procSignalGuardOnce sync.Once
	procSignalGuard     chan os.Signal
)

// armProcSignalGuard registers the process-wide guard exactly once (see above).
func armProcSignalGuard() {
	procSignalGuardOnce.Do(func() {
		procSignalGuard = make(chan os.Signal, 1)
		signal.Notify(procSignalGuard, syscall.SIGINT, syscall.SIGTERM)
	})
}

// run executes the server. It parses CLI flags first so --help/--version
// return immediately instead of starting the server, then falls through to
// the env-driven configuration and server startup. It returns a process
// exit code and is the testable entrypoint (main() is a thin wrapper), so
// TestServerHealth can invoke it without os.Args carrying go test's flags.
func run(args []string) int {
	// Arm the process-wide SIGINT/SIGTERM handler BEFORE anything else
	// (QA-CRIER-17). Every in-process user of run() — the startTestServer /
	// bootDocsClaimsServer helpers here and in the sibling test files, and
	// any embedder — shuts a booted server down by signalling the PROCESS,
	// because that is the only handle they have on it. That contract is only
	// safe while SOME run() has registered a handler: until then SIGTERM
	// takes its default action and kills the caller. Several paths below
	// return early (bad flags, load configuration, an unreachable database,
	// an invalid guard URL, a federation hold queue that cannot be opened),
	// so registering at the old site — after all of them, just before the
	// serve loop — left the window open, and a caller that signalled after
	// such a return died with no failure line: the testing package cannot
	// flush a dead process's buffered output, which is why the failure
	// showed up as a bare package FAIL with no test name.
	//
	// The channel is buffered (size 1) precisely so a signal that arrives
	// before the shutdown goroutine below is started is delivered to it
	// rather than dropped, so moving this call earlier changes no ordering
	// guarantee the shutdown path relied on.
	//
	// CI-018 finishes the job: the per-run channel is now STOPPED when its
	// run() returns, so a returned run() no longer holds a registration for
	// the life of the process. That would reopen the window QA-CRIER-17
	// closed, so the one process-lifetime registration above is armed first
	// and never stopped.
	armProcSignalGuard()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// Stop THIS run's registration the moment run() returns (CI-018): a
	// returned run() must not keep consuming process signals meant for a
	// later boot. The ordering is safe by construction on the serving path —
	// run() returns only after srv.Serve() returns, Serve() returns only once
	// srv.Shutdown() has closed the listener, and Shutdown() runs only in the
	// wait goroutine below AFTER that goroutine consumed sigCh — so by the
	// time this deferred Stop runs there is no remaining consumer on either
	// path. It is only safe because the process-wide guard above is armed
	// first: measured, this defer alone kills the QA-CRIER-17 fixture's child
	// with "signal: terminated", and with the guard it stays green.
	defer signal.Stop(sigCh)

	// Subcommands (CR-FEAT-027). `crier keygen` owns its own flag set and never
	// starts the server, so it is dispatched here — before parseArgs — and every
	// other argument shape keeps the pre-existing flag behaviour unchanged (a
	// bare `crier`, `crier -port 9001`, `crier -help` never lands here).
	if len(args) > 0 && args[0] == "keygen" {
		return runKeygen(args[1:], os.Stdout)
	}

	help, showVersion, stop, port, dbURL, pidfilePath, err := parseArgs(args, os.Stdout)
	if err != nil {
		// The flag package already printed the error and usage to stdout.
		return 2
	}
	if help {
		return 0
	}
	if showVersion {
		// The canonical build identity, e.g. "crier v1.2.3-1a2b3c4d" or, for
		// an unstamped build from a git checkout, "crier vdev-1a2b3c4d-dirty".
		fmt.Fprintf(os.Stdout, "crier %s\n", buildinfo.String())
		return 0
	}
	if stop {
		// -stop never starts the server: it resolves the pidfile (flag
		// wins over CR_PIDFILE), performs the safe stop, and exits.
		return stopServer(os.Stdout, resolvePidfile(pidfilePath))
	}

	// Flag overrides take precedence over the env-driven configuration.
	// Zero/empty values mean "not set" — the env vars win in that case.
	if port > 0 {
		os.Setenv("CRIER_PORT", strconv.Itoa(port))
	}
	if dbURL != "" {
		os.Setenv("CR_DATABASE_URL", dbURL)
	}
	pfPath := resolvePidfile(pidfilePath)

	// Bootstrap with a default info-level text logger so any pre-config
	// logging has a destination. Re-initialized with the user's choices
	// after cfg.Load() below.
	initLogger(config.Config{LogLevel: "info", LogFormat: "text"})

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		return 1
	}

	// Re-install the logger now that we know the user's level/format.
	initLogger(cfg)

	// Which registry backend serves is decided ONCE, from the configuration,
	// by registryBackendForURL: the store selection below and the GET /status
	// body (DF-CRIER-113) both key off this value, so the backend it advertises
	// cannot drift from the store that actually answers.
	regBackend := registryBackendForURL(cfg.Database.URL)

	// Configure the WebSocket origin check PER LANE (DF-CRIER-287). They are
	// different clients: the relay is subscribed to from browsers (whose
	// allowlist may reasonably require an Origin), while every mesh client is
	// an agent that sends no Origin header at all. CR_WS_ALLOWED_ORIGINS
	// therefore keeps its relay semantics and CR_MESH_ALLOWED_ORIGINS is the
	// mesh's own, explicit, opt-in policy — unset, the mesh allows every
	// origin exactly as before.
	wsCheck := config.BuildCheckOrigin(cfg.WSAllowedOrigins)
	relay.SetWSCheckOrigin(wsCheck)
	mesh.SetWSCheckOrigin(config.BuildMeshCheckOrigin(cfg.MeshAllowedOrigins))

	r := mux.NewRouter()

	// Middleware. RequestID is registered FIRST so it is the outermost
	// wrapper (gorilla/mux applies the first Use on the outside): every
	// request — including 401s and the public /health and /version routes —
	// gets a correlation id, echoed as X-Request-Id and carried in the
	// request context for the handler log lines (DF-CRIER-141).
	r.Use(middleware.RequestID)
	r.Use(middleware.Auth(cfg.AuthToken))
	r.Use(middleware.Recovery)
	r.Use(middleware.Logging)

	// Router-default 404/405 answers (DF-CRIER-213). gorilla/mux applies the
	// Use chain above only to MATCHED route handlers, so its two defaults —
	// net/http's text/plain "404 page not found" and a body-less 405 — ran
	// with no X-Request-Id and no access-log line on an API that is JSON
	// everywhere else. The fallbacks replace both and carry their own
	// RequestID/Logging wrapping; see fallback.go for why Auth and Recovery
	// are deliberately not part of that wrapping.
	registerRouterFallbacks(r)

	// Health check. The body is JSON and docs/openapi.yaml declares the 200
	// response as application/json, so the header is set explicitly: without
	// it net/http sniffs the body and labels a documented JSON resource
	// "text/plain; charset=utf-8" (DF-CRIER-102).
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}).Methods("GET")

	// Build identity (DF-CRIER-101/127) — the running binary's version,
	// commit, build time and dirty flag as JSON. Exempt from auth like
	// /health so an operator can ask a live server what it is running
	// without holding a token.
	r.HandleFunc("/version", handleVersion).Methods("GET")

	// Effective runtime posture (DF-CRIER-113): auth, per-agent signing
	// requirement, guard switch, registry backend, federation/webhook modes
	// and the build identity as JSON — the operator's answer to "what is
	// actually in force on this live server?", with no secret value in it.
	// Deliberately NOT auth-exempt: unlike /health and /version (which stay
	// public so a token-less operator can ask what a server is) this one
	// reports whether auth is ENFORCED, so an unauthenticated caller must not
	// be able to read the posture of a server that has auth on. The exempt
	// list in internal/middleware/auth.go is unchanged.
	//
	// Its registration moved down to the registry-routes block (CR-FEAT-035):
	// the body now carries the live inbox queue depth, read from the serving
	// store, and the store does not exist yet at this point in the wiring.

	// OpenAPI spec (CR-GAP-049) — the spec is served live so spec-vs-code
	// drift is visible on the running server. These three paths plus
	// /health and /version are the five auth-exempt paths (the switch in
	// internal/middleware/auth.go) so a token-less curl works.
	r.HandleFunc("/openapi.json", handleOpenAPIJSON).Methods("GET")
	r.HandleFunc("/openapi.yaml", handleOpenAPIYAML).Methods("GET")
	r.HandleFunc("/docs", handleOpenAPIDocs).Methods("GET")

	// Relay pub/sub
	relaySvc := relay.New(cfg.RateLimitPerMinute)
	// Realms (CR-FEAT-029): the namespace policy set built from
	// CR_NAMESPACES / CR_NAMESPACES_FILE, resolved ONCE here and shared by both
	// lanes that must agree about it (the relay's publish/subscribe scoping and
	// per-realm rate limits, and the registry's registration/delivery/guard/
	// retention rules). A broken document fails the boot — a realm that cannot
	// be read must never be served as a half-configured one.
	nsReg, err := buildNamespaces(cfg)
	if err != nil {
		slog.Error("initialize namespaces", "error", err)
		return 1
	}
	nsReg.LogPolicies()
	relaySvc.SetNamespacePolicies(nsReg)
	r.HandleFunc("/relay/publish", relaySvc.HandlePublish).Methods("POST")
	r.HandleFunc("/relay/subscribe/{topic}", relaySvc.HandleSubscribe)
	r.HandleFunc("/relay/topics", relaySvc.HandleTopics).Methods("GET")

	// P2P mesh
	meshCfg := mesh.DefaultMeshConfig("crier")
	// Mesh connect authentication (DF-CRIER-287): with CR_REQUIRE_MESH_AUTH
	// set, /mesh/connect/{agentID} verifies an ed25519 challenge before the
	// connection becomes a peer. The key source is wired below, once the
	// registry store exists — see registryKeyProvider.
	meshCfg.Auth = mesh.MeshAuthConfig{
		Required: cfg.RequireMeshAuth,
		Timeout:  cfg.MeshAuthTimeout,
	}
	meshSvc := mesh.NewMesh(meshCfg)
	r.HandleFunc("/mesh/connect/{agentID}", mesh.HandleConnect(meshSvc))
	r.HandleFunc("/mesh/peers", mesh.HandlePeers(meshSvc)).Methods("GET")

	// Agent registry + inboxes
	var regStore registry.Store

	if regBackend == registryBackendPostgres {
		startupCtx, cancelStartup := context.WithTimeout(context.Background(), cfg.Database.ConnectTimeout)
		defer cancelStartup()

		pgStore, err := registry.NewPostgresStoreWithPoolConfig(startupCtx, cfg.Database.URL, registry.PoolConfig{
			MaxConns:        cfg.Database.MaxConns,
			MinConns:        cfg.Database.MinConns,
			MaxConnLifetime: cfg.Database.MaxConnLifetime,
			MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
		})
		if err != nil {
			slog.Error("initialize PostgreSQL registry store", "error", err)
			return 1
		}
		defer pgStore.Close()
		regStore = pgStore
		slog.Info("registry backend", "type", "postgres", "max_conns", cfg.Database.MaxConns)
	} else {
		regStore = registry.NewMemoryStore()
		logMemoryBackendWarning()
	}

	registryHandler := registry.NewHandler(regStore)
	registryHandler.SetMaxInboxBodyBytes(cfg.InboxMaxBodyBytes)
	registryHandler.SetRequireAgentSig(cfg.RequireAgentSig)
	// Global ingest budget (CR-FEAT-035): a shed on the delivery path itself,
	// so a runaway producer cannot push every inbox deeper without limit.
	// 0 (the default) installs nothing — the delivery path is then exactly what
	// it was before this feature existed, and the per-agent publish cap stays
	// the only backpressure. When set, an over-budget delivery is refused 429
	// with a named error and a Retry-After before any transport, guard call or
	// store write.
	registryHandler.SetGlobalRateLimit(cfg.GlobalRateLimitPerMinute)
	if cfg.GlobalRateLimitPerMinute > 0 {
		slog.Info("inbox ingest budget", "global_per_minute", cfg.GlobalRateLimitPerMinute,
			"shed", "429 RATE_LIMITED_GLOBAL + Retry-After, before any transport or store work")
	} else {
		slog.Info("inbox ingest budget", "global_per_minute", 0,
			"detail", "no global budget: the per-agent publish cap (CR_RATE_LIMIT_PER_MINUTE) remains the only backpressure, exactly as before CR-FEAT-035")
	}
	// Realms (CR-FEAT-029): the registry lane enforces the same policy set the
	// relay was given — registration validates the realm and its auth posture,
	// delivery pins every message to the TARGET's realm, and the realm supplies
	// the retention default and the guard settings for deliveries into it.
	registryHandler.SetNamespacePolicies(nsReg)
	// Presence (CR-FEAT-024): the status every registry read reports is DERIVED
	// from the row's liveness evidence (`last_seen`) and this documented window,
	// so a crashed agent goes stale instead of reporting "online" forever. The
	// window is resolved once here from the packages' own default when the
	// environment sets none.
	presence := registry.NewPresence(cfg.PresenceStaleAfter)
	registryHandler.SetPresence(presence)
	// Idempotency window (CR-FEAT-025): how long a sender-supplied
	// idempotency_key deduplicates a delivery. Resolved from the registry
	// package's own default when the environment sets none.
	registryHandler.SetIdempotencyWindow(cfg.IdempotencyWindow)
	// The evidence that feeds it: mesh sockets accepted for an agent and the
	// KEEPALIVE heartbeats on them (the sink is nil when the store cannot
	// touch — a remote proxy — in which case the mesh records nothing and this
	// is a no-op exactly as before).
	meshSvc.SetLivenessRecorder(registry.HeartbeatSink(regStore))
	if presence.StaleAfter() < meshCfg.KeepaliveInterval {
		// A window shorter than the heartbeat interval flips a HEALTHY agent
		// to stale between two beats. It is a legal configuration (a test or a
		// demo wants a tiny window), so it is a warning rather than a
		// rejection — but it is never a silent one, because the symptom
		// otherwise looks like agents dying on their own.
		slog.Warn("presence: staleness window is shorter than the mesh keepalive interval — a live agent will be reported stale between heartbeats",
			"stale_after", presence.StaleAfter(), "keepalive_interval", meshCfg.KeepaliveInterval)
	}
	slog.Info("presence", "stale_after", presence.StaleAfter(),
		"evidence", "mesh connect + KEEPALIVE heartbeat + signed PATCH; stored status is never rewritten (stale is derived per read)")
	// New-message ping (CR-FEAT-023): a delivery that lands in an agent's
	// durable inbox taps an agent that ASKED for it on its existing mesh
	// socket (/mesh/connect/{agentID}?inbox_notify=1). The long-poll
	// (?wait_seconds= on GET /agents/{id}/inbox) needs nothing wired — it is
	// internal to the handler.
	registryHandler.SetInboxPinger(meshSvc)

	// Mesh identity (DF-CRIER-287). The connect handshake verifies a peer
	// against the SAME registry row the inbox lane's signatures are checked
	// against, so both lanes answer "is this agent who it says it is?" from one
	// record. Wired here because the store only exists from this point on; with
	// CR_REQUIRE_MESH_AUTH set and no provider wired, the accept path refuses
	// every connect rather than admitting an unverified peer (fail closed), so
	// there is no window in which a required handshake is silently skipped.
	if cfg.RequireMeshAuth {
		meshSvc.SetAgentKeyProvider(registryKeyProvider{store: regStore})
	}
	meshAuthTimeout := cfg.MeshAuthTimeout
	if meshAuthTimeout <= 0 {
		meshAuthTimeout = mesh.DefaultMeshAuthTimeout()
	}
	if cfg.RequireMeshAuth {
		slog.Info("mesh authentication", "required", true,
			"origin_policy", config.MeshOriginPolicy(cfg.MeshAllowedOrigins),
			"challenge_timeout", meshAuthTimeout,
			"keys", "registry ed25519 public key per agent")
	} else {
		slog.Info("mesh authentication", "required", false,
			"origin_policy", config.MeshOriginPolicy(cfg.MeshAllowedOrigins),
			"detail", "the agent id in /mesh/connect/{agentID} is taken at face value: any client that can reach this port can speak as any registered agent. Set CR_REQUIRE_MESH_AUTH=true to require the ed25519 connect challenge (docs/mesh-protocol.md §Authentication)")
	}

	// LLM message guard (CR-FEAT-010) — the inbound choke point for every
	// delivery (webhook POST / inbox store). Constructed BEFORE the routes
	// are registered (spec §2.2). CR_GUARD_ENABLED=false skips the guard
	// entirely (nil filter = disabled).
	var guardFilter guard.Filter
	if cfg.Guard.Enabled {
		// CR-FEAT-014/009: the kanban card writer (fire-and-forget output
		// option, spec §8). Cards are only produced when a policy opts in via
		// policy.kanban.enabled; write failures are logged and counted, never
		// surfaced to the delivery path. CR_GUARD_KANBAN_URL switches the
		// writer to the HTTP sink (CR-FEAT-009); empty keeps the Hermes kanban
		// CLI writer — the working kanban write path on this machine.
		kanbanMode := "cli"
		var kanbanWriter guard.CardWriter
		if cfg.Guard.KanbanURL != "" {
			w := guard.NewHTTPKanbanWriter(cfg.Guard.KanbanURL)
			if w == nil {
				slog.Error("initialize message guard", "error", "invalid CR_GUARD_KANBAN_URL (http/https scheme required)")
				return 1
			}
			kanbanWriter = w
			kanbanMode = "http"
		} else {
			kanbanWriter = guard.NewHermesKanbanWriter("hermes")
		}
		gf, err := guard.New(guard.Options{
			Timeout:           cfg.Guard.Timeout,
			MaxConcurrent:     cfg.Guard.MaxConcurrent,
			CircuitThreshold:  cfg.Guard.CircuitThreshold,
			CircuitCooldown:   cfg.Guard.CircuitCooldown,
			MaxPayloadBytes:   cfg.Guard.MaxPayloadBytes,
			RenderMaxBytes:    cfg.Guard.RenderMaxBytes,
			DeepSeekBaseURL:   cfg.Guard.DeepSeekBaseURL,
			DefaultModel:      cfg.Guard.Model,
			ExtraPatterns:     cfg.Guard.ExtraPatterns,
			DefaultPolicyJSON: cfg.Guard.DefaultPolicy,
			KanbanWriter:      kanbanWriter,
			KanbanQueueSize:   cfg.Guard.KanbanQueueSize,
		})
		if err != nil {
			slog.Error("initialize message guard", "error", err)
			return 1
		}
		defer gf.Close()
		guardFilter = gf
		slog.Info("message guard", "enabled", true, "model", cfg.Guard.Model,
			"timeout", cfg.Guard.Timeout, "max_concurrent", cfg.Guard.MaxConcurrent,
			"circuit_threshold", cfg.Guard.CircuitThreshold, "kanban_queue", cfg.Guard.KanbanQueueSize,
			"kanban_writer", kanbanMode)
	} else {
		slog.Info("message guard", "enabled", false)
	}
	registryHandler.SetGuardFilter(guardFilter)

	// Relay-to-relay federation (CR-FEAT-006). When CR_FED_LINKS is set,
	// deliveries to agents unknown on this relay are forwarded to the linked
	// relays in order (first non-404 answer wins, relayed back verbatim),
	// and GET /fed/peers lists the local relay plus each linked relay with
	// its agents. The endpoint is always registered: with no links it
	// simply lists this relay alone.
	//
	// Hold queue (DF-CRIER-7, spec §8): a TRANSIENT link outage no longer
	// answers 404 — the delivery is held at the source and retried inside
	// CR_FED_MAX_HOLD_S, and only then is the sender told FEDERATION_FAILED
	// (written straight into its inbox, which cannot recurse). Hold state is
	// durable only when CR_FED_QUEUE_FILE is set; without it the queue is
	// process-lifetime, matching the in-memory registry backend.
	var (
		fedClient *federation.Client
		fedHold   *federation.HoldManager
	)
	if len(cfg.Federation.Links) > 0 {
		fedClient = federation.NewClient(cfg.Federation.Links, 0, cfg.Federation.Token)

		queueMode := "memory (process-lifetime)"
		var holdQueue federation.HoldQueue
		if cfg.Federation.QueueFile != "" {
			fq, qerr := federation.OpenFileHoldQueue(cfg.Federation.QueueFile)
			if qerr != nil {
				slog.Error("open federation hold queue", "error", qerr)
				return 1
			}
			holdQueue = fq
			queueMode = "file"
		} else {
			holdQueue = federation.NewMemoryHoldQueue()
		}
		fedHold = federation.NewHoldManager(fedClient, holdQueue, federation.HoldConfig{
			MaxHold: cfg.Federation.MaxHold,
		})
		fedHold.SetNotifier(registry.FederationFailureSink(regStore))
		fedClient.SetHoldManager(fedHold)
		fedHold.Start()
		defer fedHold.Stop()

		registryHandler.SetFederationClient(fedClient)
		// The relay's own identity for /fed/peers (DF-CRIER-12): a
		// CR_FED_LINKS entry that addresses this relay (its own port on a
		// local-spelling host) is not a remote peer, so it must not be
		// listed a second time. Report it once here, at startup, so an
		// operator can see why a configured link is absent from the
		// listing — the per-request skip is a Debug line.
		fedClient.SetSelf(cfg.Port)
		for _, self := range fedClient.SelfLinks() {
			slog.Warn("federation: configured link addresses this relay — omitted from /fed/peers (it is already the first entry)",
				"link", self.URL, "self_port", cfg.Port)
		}
		slog.Info("federation", "links", cfg.Federation.Links, "name", federationName(cfg),
			"auth", cfg.Federation.Token != "", "max_hold_s", int(cfg.Federation.MaxHold.Seconds()),
			"hold_queue", queueMode, "pending", fedHold.Pending())
	}
	localPeer := func() federation.Peer {
		agents := make([]federation.RemoteAgent, 0)
		for _, a := range regStore.List() {
			agents = append(agents, federation.RemoteAgent{ID: a.ID, Capabilities: a.Capabilities})
		}
		return federation.Peer{
			Name:   federationName(cfg),
			URL:    fmt.Sprintf("http://localhost:%d", cfg.Port),
			Agents: agents,
		}
	}
	r.HandleFunc("/fed/peers", federation.HandlePeers(fedClient, localPeer)).Methods("GET")

	// Webhook push delivery (CR-FEAT-001) — active only for agents that
	// register a webhook endpoint.
	var secret []byte
	if cfg.Webhook.Secret != "" {
		secret = []byte(cfg.Webhook.Secret)
	}
	whClient := webhook.NewClient(cfg.Webhook.Timeout, secret)
	whDriver := webhook.NewDriver(whClient, webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries:       cfg.Webhook.MaxRetries,
		RedeliverEvery:   cfg.Webhook.RedeliverEvery,
		ProbeEvery:       cfg.Webhook.ProbeEvery,
		CircuitThreshold: cfg.Webhook.CircuitThreshold,
	})
	whDriver.SetConfigResolver(func(agentID string) (*webhook.Config, error) {
		agent, err := regStore.Get(agentID)
		if err != nil {
			return nil, err
		}
		if agent.Webhook == nil {
			return nil, fmt.Errorf("agent %s has no webhook configured", agentID)
		}
		return agent.Webhook, nil
	})
	// DF-CRIER-8: when an async delivery exhausts its bounded retries, the
	// originating sender gets exactly one durable WEBHOOK_FAILED
	// notification in its own inbox. The sink is the Store's direct inbox
	// Deliver path — it bypasses webhook routing, so it cannot recurse.
	whDriver.SetFailureNotifier(registry.WebhookFailureSink(regStore))
	whDriver.Start()
	defer whDriver.Stop()
	registryHandler.SetWebhookDriver(whDriver)
	slog.Info("webhook delivery", "enabled", true, "timeout", cfg.Webhook.Timeout,
		"retries", cfg.Webhook.MaxRetries, "signing", secret != nil)
	r.HandleFunc("/agents", registryHandler.HandleRegister).Methods("POST")
	r.HandleFunc("/agents", registryHandler.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", registryHandler.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", registryHandler.HandleUnregister).Methods("DELETE")
	r.HandleFunc("/agents/{id}", registryHandler.HandleUpdateAgent).Methods("PATCH")
	r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleDeliver).Methods("POST")
	// Capability-routed delivery (CR-FEAT-026): the registry's advertised
	// capabilities become a dialable worker pool. The path names the CAPABILITY
	// where the by-id route names an agent, and the holder is chosen from the
	// live holders by round-robin (registry/capability.go); everything after
	// that choice is the deliver path above, unchanged. A capability nobody
	// holds is refused with 404 NO_CAPABLE_AGENT, never dropped.
	r.HandleFunc("/capabilities/{capability}/inbox", registryHandler.HandleDeliverByCapability).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/ack", registryHandler.HandleAck).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/stats", registryHandler.HandleStats).Methods("GET")
	// Task ownership (CR-FEAT-025): the transfer/reassign surface for a stuck
	// lease, and the dead-letter destination a TTL expiry feeds.
	r.HandleFunc("/agents/{id}/inbox/transfer", registryHandler.HandleTransfer).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/dead-letters", registryHandler.HandleDeadLetters).Methods("GET")
	// Realms (CR-FEAT-029): the policy set this server is actually enforcing,
	// with a live per-namespace agent census. Registered unconditionally — an
	// unconfigured deployment answers with the one implicit namespace — and
	// NOT auth-exempt: like /status it reports posture, so a token-less caller
	// must not be able to read it on a server that has auth on.
	r.HandleFunc("/namespaces", registryHandler.HandleListNamespaces).Methods("GET")

	// Effective runtime posture + the LIVE queue depth (DF-CRIER-113,
	// CR-FEAT-035). Registered here rather than with /health, /version and the
	// spec-hosting routes above because its body now carries a measurement of
	// the serving store — how much is queued, how much of that is leased, how
	// old the oldest waiting message is — and this is the first point in the
	// wiring at which that store exists. Nothing else about the endpoint
	// changes: it is still authenticated, still not on the auth-exempt list
	// (internal/middleware/auth.go), and still reports booleans and modes for
	// every configuration fact.
	r.HandleFunc("/status", newStatusHandlerWithQueue(cfg, regBackend, newQueueDepthReader(regStore))).Methods("GET")

	// A2A Agent Card discovery (INT-A2A-002, specs/A2A-OPTION.md §5.2) — the
	// OPT-IN extra, and the only A2A surface any row of this series has
	// registered so far. It exists ONLY while CR_A2A_ENABLED is true, so with
	// the switch unset the path is unregistered and answers exactly what it
	// answered before the A2A option existed (the router's JSON 404). With it
	// set, the route serves a card for an agent whose registry row opted in
	// (`a2a.enabled`) and refuses every other id with 404 — an agent that did
	// not opt in is never advertised.
	//
	// Nothing existing moves: the route is registered by registerA2ACardRoute
	// (cmd/server/a2acard.go) rather than here, and it inherits the middleware
	// chain unchanged — the auth-exempt list is untouched, so with
	// CR_AUTH_TOKEN set it requires the same Bearer header as every other route.
	if cfg.A2AEnabled {
		registerA2ACardRoute(r, regStore, a2aCardOptions{
			port:                   cfg.Port,
			authTokenSet:           cfg.AuthToken != "",
			agentSignatureEnforced: cfg.RequireAgentSig,
		})
		// The JSON-RPC binding (INT-A2A-003 §5.4, extended by INT-A2A-004 §5.5 and
		// INT-A2A-005): SendMessage, SendStreamingMessage, the task lifecycle —
		// GetTask, ListTasks, CancelTask, SubscribeToTask — and the four
		// push-notification configuration methods, at the path the card above
		// advertises. A send and a lifecycle operation reuse the registry
		// handler's own HandleDeliver — the very function POST /agents/{id}/inbox
		// is registered with — so an A2A send is an ordinary crier delivery
		// through the guard, idempotency, detection, federation, webhook and inbox
		// paths; the lifecycle reads and closes the SAME inbox entry through the
		// store's read-only and close capabilities; and a push configuration is an
		// ordinary crier webhook update (HandleUpdateAgent). Nothing about
		// delivery or configuration is re-implemented for A2A.
		registerA2ARoute(r, regStore, registryHandler.HandleDeliver, registryHandler.HandleUpdateAgent, relaySvc, a2aOptions{
			port:                   cfg.Port,
			authTokenSet:           cfg.AuthToken != "",
			agentSignatureEnforced: cfg.RequireAgentSig,
		})
		slog.Info("A2A option enabled", "route", a2a.AgentCardPath,
			"serves", "an AgentCard for agents whose registry row opted in (\"a2a\":{\"enabled\":true}); every other id answers 404",
			"json_rpc_route", a2a.JSONRPCBindingPath,
			"methods", a2a.MethodSendMessage+" (a Task or a direct Message), "+
				a2a.MethodSendStreamingMessage+" (text/event-stream), "+
				a2a.MethodGetTask+", "+a2a.MethodListTasks+", "+
				a2a.MethodCancelTask+", "+a2a.MethodSubscribeToTask+
				" and the push-notification configuration methods "+
				a2a.MethodCreateTaskPushNotificationConfig+"/"+a2a.MethodGetTaskPushNotificationConfig+"/"+
				a2a.MethodListTaskPushNotificationConfigs+"/"+a2a.MethodDeleteTaskPushNotificationConfig+
				" (a view over the agent's existing webhook config)",
			"not_registered", "GetExtendedAgentCard (INT-A2A-006)")
	} else {
		slog.Info("A2A option disabled", "detail",
			"no A2A route is registered; set CR_A2A_ENABLED=true to publish the Agent Card discovery route and the JSON-RPC binding")

	}

	// Detection & containment (CR-FEAT-030, specs/DETECTION.md) — OPT-IN.
	//
	// The bus could always ATTRIBUTE a delivery after the fact (ed25519
	// identity, the single delivery choke point, the failure receipts). This
	// block is what lets an operator see an intrusion WHILE it happens and end
	// it in one call: an append-only signed delivery log, per-sender behaviour
	// baselines that raise alerts, a kill-switch and canary tokens.
	//
	// Off by default: with CR_DETECT_ENABLED unset nothing here runs — no
	// route is registered, no file is written, and the delivery path is
	// byte-identical to a build without this feature. When it IS on and the
	// configured log cannot be verified (a rewritten or torn record), startup
	// FAILS rather than appending to a history that no longer means anything.
	if cfg.Detection.Enabled {
		det, err := detect.New(detect.Config{
			LogPath:           cfg.Detection.LogPath,
			KeyPath:           cfg.Detection.KeyPath,
			FanoutWindow:      cfg.Detection.FanoutWindow,
			FanoutMinTargets:  cfg.Detection.FanoutMinTargets,
			NewPeerWindow:     cfg.Detection.NewPeerWindow,
			NewPeerMinTargets: cfg.Detection.NewPeerMinTargets,
			QuietStartHour:    cfg.Detection.QuietStartHour,
			QuietEndHour:      cfg.Detection.QuietEndHour,
			QuietMinMessages:  cfg.Detection.QuietMinMessages,
			CanaryTokens:      cfg.Detection.CanaryTokens,
		})
		if err != nil {
			slog.Error("initialize detection layer", "error", err)
			return 1
		}
		defer det.Close()
		det.SetStore(regStore)
		det.SetWebhooks(whDriver)
		registryHandler.SetDetector(det)
		dcfg := det.Config()
		var keyID string
		if a := det.Audit(); a != nil {
			keyID = a.KeyID()
		}
		slog.Info("detection enabled",
			"log", cfg.Detection.LogPath, "log_key_id", keyID,
			"fanout_window_s", int(dcfg.FanoutWindow.Seconds()), "fanout_targets", dcfg.FanoutMinTargets,
			"newpeer_window_s", int(dcfg.NewPeerWindow.Seconds()), "newpeer_targets", dcfg.NewPeerMinTargets,
			"quiet_hours_utc", fmt.Sprintf("%02d:00-%02d:00", dcfg.QuietStartHour, dcfg.QuietEndHour),
			"quiet_min_messages", dcfg.QuietMinMessages,
			"canaries", len(det.Canaries()))
		// The canary tokens are printed because they are PLANTED secrets: an
		// operator has to know them to put one where an exfiltrating agent
		// would find it. That is the feature, not a leak — but it is also why
		// the tokens rotate per boot unless CR_CANARY_TOKENS pins them.
		for _, c := range det.Canaries() {
			slog.Info("canary planted", "id", c.ID, "token", c.Token)
		}
		dh := &detectionHandlers{det: det}
		r.HandleFunc("/delivery-log", dh.HandleDeliveryLog).Methods("GET")
		r.HandleFunc("/delivery-log/verify", dh.HandleDeliveryLogVerify).Methods("GET")
		r.HandleFunc("/alerts", dh.HandleAlerts).Methods("GET")
		r.HandleFunc("/canaries", dh.HandleCanaries).Methods("GET")
		r.HandleFunc("/agents/{id}/kill-switch", dh.HandleKillSwitch).Methods("POST")
	}

	// Opt-in live-inspection surfaces (DF-CRIER-142): GET /metrics and
	// GET /debug/pprof/* exist ONLY when CR_ENABLE_METRICS /
	// CR_ENABLE_PPROF is set; unset flags leave both 404. Neither is
	// auth-exempt. This must run after relay/mesh/federation exist (the
	// gauges read them) and returns the handler the server serves (the
	// metrics flag wraps the mux with the request counter).
	srvHandler := registerObservability(r, observabilityFlags{
		EnableMetrics: cfg.Observability.EnableMetrics,
		EnablePProf:   cfg.Observability.EnablePProf,
	}, observabilityDeps{
		relaySvc: relaySvc,
		meshSvc:  meshSvc,
		fedHold:  fedHold,
		// The inbox queue gauges read the store that actually serves
		// (CR-FEAT-035). A store that cannot report a depth leaves them NaN.
		queueDepth: newQueueDepthReader(regStore),
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      srvHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	// Periodic expired message purging. The HANDLER's sweep, not the store's
	// bare PurgeExpired (CR-FEAT-025): it dead-letters every message it removes
	// and writes exactly one MESSAGE_EXPIRED receipt into each one's sender
	// inbox, so a TTL expiry is a reported outcome instead of a silent delete.
	// The store's count-only PurgeExpired remains the whole contract for a
	// backend that cannot report.
	purgeCtx, purgeCancel := context.WithCancel(context.Background())
	defer purgeCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-purgeCtx.Done():
				return
			case <-ticker.C:
				registryHandler.PurgeExpired()
			}
		}
	}()

	// Graceful shutdown. The signal handler was registered at the TOP of run
	// (QA-CRIER-17) so it is installed for every path that can return early,
	// not just the ones that get this far; the wait goroutine starts here,
	// after srv/regStore/fedHold exist, and the buffered channel holds a
	// signal that arrived while the server was still being wired up.
	//
	// The pidfile is REMOVED on this same graceful path (DF-CRIER-194): by
	// the time the shutdown finishes, the listener is gone, so the pidfile
	// must not outlive the server it names. It is written only after the
	// bind succeeded (see the listen call below).
	go func() {
		sig := <-sigCh
		logShutdownSignal(sig, cfg.Port)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		purgeCancel()
		meshSvc.Stop()
		if fedHold != nil {
			// Stop the federation retry worker before the store closes: it
			// joins its goroutine, so shutdown leaks nothing. Pending items
			// stay in the queue (durable when CR_FED_QUEUE_FILE is set).
			fedHold.Stop()
		}
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("http shutdown", "error", err)
		}
		if pfPath != "" {
			if err := pidfile.Remove(pfPath); err != nil {
				slog.Warn("remove pidfile", "error", err)
			}
		}
		if closer, ok := regStore.(interface{ Close() }); ok {
			closer.Close()
		}
	}()

	slog.Info("crier starting", "port", cfg.Port, "services", "relay+mesh+registry",
		"version", buildinfo.String())
	// Bind BEFORE serving: the pidfile must exist only once the port is
	// actually held, and a failed bind must leave no pidfile behind
	// (DF-CRIER-194). The existing logServeFailure diagnostic for the
	// shared-port dead end (DF-CRIER-154) keeps its shape and gains a
	// report of the pidfile already on disk at pfPath when there is one
	// (DF-CRIER-283) — a failed start leaves that file untouched, so the
	// operator has to be told whether it still names a live server.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		logServeFailure(cfg.Port, err, pfPath)
		return 1
	}
	if pfPath != "" {
		self, exeErr := os.Executable()
		if exeErr != nil {
			slog.Warn("pidfile: resolve binary path", "error", exeErr)
		} else {
			if err := pidfile.Write(pfPath, pidfile.Record{PID: os.Getpid(), Port: cfg.Port, Binary: self}); err != nil {
				slog.Warn("pidfile: write", "path", pfPath, "error", err)
			} else {
				slog.Info("pidfile written", "path", pfPath)
			}
		}
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logServeFailure(cfg.Port, err, pfPath)
		return 1
	}
	return 0
}

// logShutdownSignal records the signal that took this server down gracefully
// (CI-018).
//
// The graceful path was the ONLY silent early exit of run(): the wait
// goroutine calls srv.Shutdown, srv.Serve then returns http.ErrServerClosed,
// and run() returns 0 without a log line of its own. An in-process test whose
// server was signalled while it was still booting therefore failed with
// "server exited before answering /health" and a captured log holding ONLY the
// normal startup lines — the exit REASON was nowhere in the output. This one
// line is what makes that exit attributable: which signal, on which port, for
// which build. It is production diagnostics too (an operator reading a
// container's log sees why the process went away).
//
// The body is deliberately a one-liner over slog so a test can intercept
// slog.SetDefault and read the exact shape that ships.
func logShutdownSignal(sig os.Signal, port int) {
	slog.Info("shutdown signal received", "signal", sig.String(), "port", port, "version", buildinfo.String())
}

// logServeFailure reports a fatal ListenAndServe error. For the shared-host
// dead end (DF-CRIER-154) — the port already held by a leftover server from
// an earlier session — it turns the raw errno into the operator's next two
// commands instead: who holds the port, and how to start elsewhere. The
// build identity is repeated on the failure path so the log says WHICH
// binary failed, even when the "crier starting" line above scrolled away or
// was filtered out. The raw error stays in error= unchanged; any other
// failure keeps the one-line shape it always had.
//
// When a pidfile path is configured, the same failure also says what that
// pidfile now names (DF-CRIER-283). The file is written only after a bind
// SUCCEEDS, so a failed start leaves the previous one on disk — plausible,
// authoritative-looking, and impossible to read as a pid — and an operator
// who reads only the log cannot tell a still-serving predecessor from a
// stale record. The pidfile contributes attributes only when the file
// actually exists: no path configured, or nothing on disk, and the
// diagnostic keeps the exact shape DF-CRIER-154 established.
func logServeFailure(port int, err error, pfPath string) {
	if !errors.Is(err, syscall.EADDRINUSE) {
		slog.Error("server failed", "error", err)
		return
	}
	args := []any{
		"error", err,
		"port", port,
		"hint_holder", fmt.Sprintf("find it with: ss -tlnp | grep :%d", port),
		"hint_run_elsewhere", "start on a free port instead: -port <n> (or set CRIER_PORT=<n>)",
	}
	args = append(args, pidfileFailureAttrs(pfPath)...)
	args = append(args, "version", buildinfo.String())
	slog.Error("server failed: another process already holds this port "+
		"(bind: address already in use)", args...)
}

// stopCommandFor is the exact operator command that stops the server a
// pidfile names. It is spelled out on the failure path (DF-CRIER-283)
// because the pidfile itself cannot be fed to kill — it is a JSON document,
// not a pid, so `kill $(cat .crier.pid)` hands bash the literal "{". The same
// command is what `make stop` runs and what README's Stop / restart section
// documents.
func stopCommandFor(path string) string {
	return "./bin/crier -stop -pidfile " + path
}

// pidfileFailureAttrs inspects the pidfile at path after a failed bind and
// returns the attributes that tell the operator what the file names. The
// pidfile survives a failed start, so the four shapes below must not blur
// into each other — each one is a different next move for the operator:
//
//   - no path configured, or no file on disk: no attributes at all, because
//     there is nothing to explain (the DF-CRIER-154 diagnostic is unchanged);
//   - the recorded pid is alive and IS the recorded binary: this start did
//     not take the port over. That file is authoritative, so the message
//     names the pid and port and prints the exact stop command;
//   - the recorded pid is not running: the file is stale, and there is no
//     server of this pidfile to stop — saying so is required precisely
//     because suggesting -stop here would send the operator after a dead
//     process;
//   - the file cannot be read, or its pid is alive but runs a DIFFERENT
//     binary (or /proc cannot be read): the file cannot be acted on. It is
//     reported with both paths and no stop is suggested, the same fail-closed
//     stance pidfile.SafeToSignal takes for -stop.
func pidfileFailureAttrs(path string) []any {
	if path == "" {
		return nil
	}
	rec, err := pidfile.Read(path)
	if err != nil {
		if errors.Is(err, pidfile.ErrNoPidfile) {
			return nil // nothing on disk: nothing to explain
		}
		return []any{
			"pidfile", path,
			"pidfile_state", "unreadable",
			"hint_pidfile", fmt.Sprintf("the pidfile at %s could not be read (%v) — it is not usable state; inspect or remove it before trusting it", path, err),
		}
	}

	sigErr := pidfile.SafeToSignal(rec)
	switch {
	case sigErr == nil:
		return []any{
			"pidfile", path,
			"pidfile_state", "live",
			"pidfile_pid", rec.PID,
			"pidfile_port", rec.Port,
			"hint_takeover", fmt.Sprintf(
				"pidfile %s still names a LIVE server: pid %d (port %d) — this start did not take the port over and that process is still serving. Stop it with: %s",
				path, rec.PID, rec.Port, stopCommandFor(path)),
		}
	case errors.Is(sigErr, pidfile.ErrNotAlive):
		return []any{
			"pidfile", path,
			"pidfile_state", "stale",
			"pidfile_pid", rec.PID,
			"hint_stale_pidfile", fmt.Sprintf(
				"pidfile %s is STALE: pid %d (port %d) is not running — there is no server of this pidfile to stop, so do not signal that pid. The port is held by another process; find it with: ss -tlnp | grep :%d",
				path, rec.PID, rec.Port, rec.Port),
		}
	}

	var mismatch *pidfile.MismatchError
	if errors.As(sigErr, &mismatch) {
		return []any{
			"pidfile", path,
			"pidfile_state", "foreign",
			"pidfile_pid", rec.PID,
			"hint_pidfile", fmt.Sprintf(
				"pidfile %s names pid %d, which is running but is NOT the binary that file recorded — do not signal it (the -stop path refuses this case too). Recorded: %s — running: %s",
				path, rec.PID, mismatch.Recorded, mismatch.Live),
		}
	}
	return []any{
		"pidfile", path,
		"pidfile_state", "unverifiable",
		"pidfile_pid", rec.PID,
		"hint_pidfile", fmt.Sprintf(
			"pidfile %s names pid %d, which could not be verified against the kernel (%v) — do not signal it on this evidence; inspect %s",
			path, rec.PID, sigErr, path),
	}
}

// parseArgs parses crier's CLI flags, writing usage/error output to out.
// Return values:
//   - help: -help/--help (or -h) was requested; usage has already been
//     printed to out.
//   - showVersion: -version/--version was requested.
//   - stop: -stop was requested (perform the stop action and exit; the
//     server never starts).
//   - port/dbURL/pidfile: flag overrides; 0/"" mean the flag was not set
//     and the environment (or, for pidfile, nothing at all) wins.
//   - err: parse failure (unknown flag or bad value); the error message and
//     usage have already been printed to out.
func parseArgs(args []string, out io.Writer) (help, showVersion, stop bool, port int, dbURL, pidfile string, err error) {
	fs := flag.NewFlagSet("crier", flag.ContinueOnError)
	fs.SetOutput(out)
	versionFlag := fs.Bool("version", false, "print version and exit")
	portFlag := fs.Int("port", 0, "listen port (overrides CRIER_PORT)")
	dbURLFlag := fs.String("db-url", "", "PostgreSQL connection URL (overrides CR_DATABASE_URL)")
	stopFlag := fs.Bool("stop", false, "stop the server recorded in the pidfile (see -pidfile) and exit; never starts the server")
	pidfileFlag := fs.String("pidfile", "", "write a pidfile at this path once the port is bound, removed on graceful shutdown (overrides CR_PIDFILE; pairs with -stop)")
	fs.Usage = func() { printUsage(out, fs) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The flag package already printed usage. -h/-help must exit
			// 0 (the package's default ExitOnError path exits 2, which
			// violates the CR-GAP-005 acceptance criteria).
			return true, false, false, 0, "", "", nil
		}
		return false, false, false, 0, "", "", err
	}
	return false, *versionFlag, *stopFlag, *portFlag, *dbURLFlag, *pidfileFlag, nil
}

// resolvePidfile picks the pidfile path: an explicit -pidfile flag wins,
// then the CR_PIDFILE env var; empty means "no pidfile" and the server
// writes nothing (the default — behaviour only changes for operators who
// ask for it, DF-CRIER-194).
func resolvePidfile(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	return os.Getenv("CR_PIDFILE")
}

// stopServer performs the -stop action against path and returns a process
// exit code. It is the safe half of DF-CRIER-194: no pidfile is a clean
// no-op, a stale pidfile is cleaned up, a live pid whose /proc exe does
// not match the recorded binary is REFUSED without any signal, and the
// happy path sends one SIGTERM (which triggers the existing graceful
// shutdown), waits up to 10s, then removes the pidfile. It never
// SIGKILLs and never falls back to matching by port or process name — an
// unrelated process must not be reachable through this command.
func stopServer(out io.Writer, path string) int {
	if path == "" {
		fmt.Fprintln(out, "crier: -stop needs a pidfile: pass -pidfile <path> or set CR_PIDFILE")
		return 2
	}
	rec, err := pidfile.Read(path)
	if errors.Is(err, pidfile.ErrNoPidfile) {
		fmt.Fprintf(out, "crier: nothing to stop — no pidfile at %s\n", path)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "crier: cannot read pidfile: %v\n", err)
		return 1
	}

	if err := pidfile.SafeToSignal(rec); err != nil {
		if errors.Is(err, pidfile.ErrNotAlive) {
			// Stale pidfile: the server it named is already gone.
			fmt.Fprintf(out, "crier: pid %d (port %d) is not running — removing stale pidfile %s\n",
				rec.PID, rec.Port, path)
			if rmErr := pidfile.Remove(path); rmErr != nil {
				fmt.Fprintf(os.Stderr, "crier: removing stale pidfile: %v\n", rmErr)
				return 1
			}
			return 0
		}
		var mismatch *pidfile.MismatchError
		if errors.As(err, &mismatch) {
			fmt.Fprintf(os.Stderr, "crier: REFUSING to stop pid %d: it is not the server this pidfile recorded\n", rec.PID)
			fmt.Fprintf(os.Stderr, "  pidfile %s recorded binary: %s\n", path, mismatch.Recorded)
			fmt.Fprintf(os.Stderr, "  pid %d is actually running:  %s\n", rec.PID, mismatch.Live)
			fmt.Fprintf(os.Stderr, "  (the pid was likely recycled by the OS; nothing was signalled)\n")
		} else {
			fmt.Fprintf(os.Stderr, "crier: REFUSING to stop pid %d: %v (nothing was signalled)\n", rec.PID, err)
		}
		return 1
	}

	proc, findErr := os.FindProcess(rec.PID)
	if findErr != nil {
		fmt.Fprintf(os.Stderr, "crier: find pid %d: %v\n", rec.PID, findErr)
		return 1
	}
	fmt.Fprintf(out, "crier: stopping pid %d (SIGTERM)\n", rec.PID)
	if sigErr := proc.Signal(syscall.SIGTERM); sigErr != nil {
		// The process may have exited between the ownership check and
		// the signal; treat "already gone" as success, anything else
		// as a failure.
		if errors.Is(sigErr, syscall.ESRCH) || errors.Is(sigErr, os.ErrProcessDone) {
			fmt.Fprintf(out, "crier: pid %d already exited\n", rec.PID)
			_ = pidfile.Remove(path)
			return 0
		}
		fmt.Fprintf(os.Stderr, "crier: signal pid %d: %v\n", rec.PID, sigErr)
		return 1
	}

	// Poll for exit: killed-but-unreaped children (and processes whose
	// parent is not us) drop their /proc entry promptly on exit, and
	// pidfile.Alive also treats zombies as gone. Graceful shutdown takes
	// a moment (open connections drain), so allow 10s.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidfile.Alive(rec.PID) {
			fmt.Fprintf(out, "crier: pid %d stopped\n", rec.PID)
			if rmErr := pidfile.Remove(path); rmErr != nil {
				fmt.Fprintf(os.Stderr, "crier: removing pidfile: %v\n", rmErr)
				return 1
			}
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "crier: pid %d is still running after 10s — not removing %s; inspect it (ss -tlnp | grep :%d) and stop it manually if needed\n",
		rec.PID, path, rec.Port)
	return 1
}

// federationName returns this relay's display name for the /fed/peers
// listing: CR_FED_NAME when set, else localhost:<port>.
func federationName(cfg config.Config) string {
	if cfg.Federation.Name != "" {
		return cfg.Federation.Name
	}
	return fmt.Sprintf("localhost:%d", cfg.Port)
}

// logMemoryBackendWarning announces the non-durable backend at a level nobody
// can miss (CR-FEAT-034: make the durable path the documented default; pairs
// with CR-FEAT-031's startup warning).
//
// The in-memory store keeps agents and undelivered messages in process memory,
// so a restart eats the queue — while crier's product promise is a DURABLE
// inbox. Measured against the external review (DISPATCH · CRI-001): the
// first-run experience demonstrated "durability" on a backend that keeps
// nothing, and the only startup signal was an INFO line carrying a hint, read
// like every other startup line. This is a WARN that names the consequence, the
// fix and a machine-readable `durable=false`; it changes no behaviour, and
// GET /status still reports registry_backend=memory for a programmatic check.
func logMemoryBackendWarning() {
	slog.Warn("registry backend is the in-memory store — DEMO-ONLY and non-durable: agents and undelivered inbox messages are lost when this process exits. Set CR_DATABASE_URL (docker compose up -d postgres) for the durable inbox the docs document",
		"type", "memory",
		"durable", false,
		"hint", "set CR_DATABASE_URL for PostgreSQL",
	)
}

// printUsage writes the full usage text: the flag summary plus the
// environment variables crier reads for configuration.
func printUsage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(out, "crier — pub/sub relay + mesh + registry server")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  crier [flags]")
	fmt.Fprintln(out, "  crier keygen [flags]        generate an ed25519 keypair + the agent config (no openssl, no xxd)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	fs.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Configuration is read from environment variables; the")
	fmt.Fprintln(out, "-port and -db-url flags override them when set:")
	fmt.Fprintln(out, "  CRIER_PORT                  listen port (default 8767)")
	fmt.Fprintln(out, "  CR_PIDFILE                  pidfile path; set to pair the server with -stop / make stop (default: none)")
	fmt.Fprintln(out, "  CR_DATABASE_URL             PostgreSQL URL — the durable backend the docs document; unset = demo-only in-memory store (fallbacks: DATABASE_URL, CRIER_DATABASE_URL)")
	fmt.Fprintln(out, "  CR_AUTH_TOKEN               bearer token required on all requests (empty = auth disabled)")
	fmt.Fprintln(out, "  CR_REQUIRE_AGENT_SIG        require per-agent ed25519 signatures (default true)")
	fmt.Fprintln(out, "  CR_PRESENCE_STALE_AFTER_S   how long a registry row may go without liveness evidence before its status reports stale (default 90 = three missed mesh heartbeats)")
	fmt.Fprintln(out, "  CR_LOG_LEVEL                debug|info|warn|error (default info)")
	fmt.Fprintln(out, "  CR_LOG_FORMAT               text|json (default text)")
	fmt.Fprintln(out, "  CR_RATE_LIMIT_PER_MINUTE    relay publish rate limit (default 100)")
	fmt.Fprintln(out, "  CR_INBOX_MAX_BODY_BYTES     maximum raw body for inbox deliveries (default 1048576; invalid values fail startup; over-cap requests return 413 REQUEST_BODY_TOO_LARGE)")
	fmt.Fprintln(out, "  CR_WS_ALLOWED_ORIGINS       comma-separated WebSocket origins, \"*\" = allow all")
	fmt.Fprintln(out, "  CR_FED_LINKS                comma-separated base URLs of linked relays (relay federation)")
	fmt.Fprintln(out, "  CR_FED_NAME                 optional local relay name for the /fed/peers listing")
	fmt.Fprintln(out, "  CR_FED_TOKEN                shared secret for link auth: sent as Bearer to linked relays; must equal the destination's CR_AUTH_TOKEN (empty = no link auth)")
	fmt.Fprintln(out, "  CR_FED_MAX_HOLD_S           how long a delivery is held and retried when every link is down, before the sender is told FEDERATION_FAILED (default 300)")
	fmt.Fprintln(out, "  CR_FED_QUEUE_FILE           durable hold-queue document path; unset = held deliveries are process-lifetime only (lost on restart)")
	fmt.Fprintln(out, "  CR_GUARD_ENABLED            LLM message guard master switch (default true)")
	fmt.Fprintln(out, "  CR_GUARD_TIMEOUT_MS         per-message guard budget incl. retries (default 10000)")
	fmt.Fprintln(out, "  CR_GUARD_MAX_CONCURRENT     concurrent guard LLM calls (default 8)")
	fmt.Fprintln(out, "  CR_GUARD_CIRCUIT_THRESHOLD  consecutive provider failures to open the circuit (default 10)")
	fmt.Fprintln(out, "  CR_GUARD_CIRCUIT_COOLDOWN_S circuit open duration (default 300)")
	fmt.Fprintln(out, "  CR_GUARD_MAX_PAYLOAD_BYTES  payloads above this skip the LLM (default 65536)")
	fmt.Fprintln(out, "  CR_GUARD_RENDER_MAX_BYTES   projection cap fed to the LLM (default 32768)")
	fmt.Fprintln(out, "  CR_GUARD_DEEPSEEK_BASE_URL  deepseek preset base URL override (default https://api.deepseek.com/v1)")
	fmt.Fprintln(out, "  CR_GUARD_MODEL              deepseek preset default model override (default deepseek-v4-flash)")
	fmt.Fprintln(out, "  CR_GUARD_PATTERNS_EXTRA     JSON array of extra prematch patterns (append/replace)")
	fmt.Fprintln(out, "  CR_GUARD_DEFAULT_POLICY     JSON Policy — server-wide default when the agent has none (fail-fast)")
	fmt.Fprintln(out, "  CR_GUARD_KANBAN_QUEUE       kanban worker queue capacity, opt-in per policy.kanban (default 100)")
	fmt.Fprintln(out, "  CR_GUARD_KANBAN_URL         HTTP kanban sink base URL; empty = hermes kanban CLI writer (default empty)")
	fmt.Fprintln(out, "  CR_ENABLE_PPROF             opt-in: register GET /debug/pprof/* (default false = 404; not auth-exempt)")
	fmt.Fprintln(out, "  CR_ENABLE_METRICS           opt-in: register GET /metrics, Prometheus text format (default false = 404; not auth-exempt)")
	fmt.Fprintln(out, "  CR_DETECT_ENABLED           opt-in: signed delivery log + behaviour alerts + kill-switch + canaries, and the five detection routes (default false = 404; not auth-exempt)")
	fmt.Fprintln(out, "  CR_DETECT_LOG               append-only signed delivery log path (empty = no log; alerts and containment still work)")
	fmt.Fprintln(out, "  CR_DETECT_KEY               ed25519 signing key file for the log (default <CR_DETECT_LOG>.key, created 0600)")
	fmt.Fprintln(out, "  CR_DETECT_FANOUT_WINDOW_S   fan-out signal window (default 60)")
	fmt.Fprintln(out, "  CR_DETECT_FANOUT_MIN_TARGETS distinct targets in that window to trip fanout_spike (default 5)")
	fmt.Fprintln(out, "  CR_DETECT_NEWPEER_WINDOW_S  new-peer signal window (default 60)")
	fmt.Fprintln(out, "  CR_DETECT_NEWPEER_MIN_TARGETS first-ever conversations in that window to trip new_peer_burst (default 3)")
	fmt.Fprintln(out, "  CR_DETECT_QUIET_HOURS       UTC quiet window S-E, start inclusive end exclusive, may wrap (default 1-5)")
	fmt.Fprintln(out, "  CR_DETECT_QUIET_MIN_MESSAGES messages from one sender in the quiet window to trip odd_hour_volume (default 3)")
	fmt.Fprintln(out, "  CR_CANARY_TOKENS            comma-separated canary tokens to plant; empty = the server generates them at boot and logs them")
	fmt.Fprintln(out, "  DEEPSEEK_API_KEY            deepseek preset API key (env:DEEPSEEK_API_KEY ref)")
	fmt.Fprintln(out, "  CR_DATABASE_*               PostgreSQL pool tuning (MAX_CONNS, MIN_CONNS, ...)")
}
