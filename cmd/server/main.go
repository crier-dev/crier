package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/middleware"
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

// run executes the server. It parses CLI flags first so --help/--version
// return immediately instead of starting the server, then falls through to
// the env-driven configuration and server startup. It returns a process
// exit code and is the testable entrypoint (main() is a thin wrapper), so
// TestServerHealth can invoke it without os.Args carrying go test's flags.
func run(args []string) int {
	help, showVersion, port, dbURL, err := parseArgs(args, os.Stdout)
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

	// Flag overrides take precedence over the env-driven configuration.
	// Zero/empty values mean "not set" — the env vars win in that case.
	if port > 0 {
		os.Setenv("CRIER_PORT", strconv.Itoa(port))
	}
	if dbURL != "" {
		os.Setenv("CR_DATABASE_URL", dbURL)
	}

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

	// Configure WebSocket origin check for relay and mesh.
	wsCheck := config.BuildCheckOrigin(cfg.WSAllowedOrigins)
	relay.SetWSCheckOrigin(wsCheck)
	mesh.SetWSCheckOrigin(wsCheck)

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

	// Health check
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}).Methods("GET")

	// Build identity (DF-CRIER-101/127) — the running binary's version,
	// commit, build time and dirty flag as JSON. Exempt from auth like
	// /health so an operator can ask a live server what it is running
	// without holding a token.
	r.HandleFunc("/version", handleVersion).Methods("GET")

	// OpenAPI spec (CR-GAP-049) — the spec is served live so spec-vs-code
	// drift is visible on the running server. These three paths plus
	// /health and /version are the five auth-exempt paths (the switch in
	// internal/middleware/auth.go) so a token-less curl works.
	r.HandleFunc("/openapi.json", handleOpenAPIJSON).Methods("GET")
	r.HandleFunc("/openapi.yaml", handleOpenAPIYAML).Methods("GET")
	r.HandleFunc("/docs", handleOpenAPIDocs).Methods("GET")

	// Relay pub/sub
	relaySvc := relay.New(cfg.RateLimitPerMinute)
	r.HandleFunc("/relay/publish", relaySvc.HandlePublish).Methods("POST")
	r.HandleFunc("/relay/subscribe/{topic}", relaySvc.HandleSubscribe)
	r.HandleFunc("/relay/topics", relaySvc.HandleTopics).Methods("GET")

	// P2P mesh
	meshCfg := mesh.DefaultMeshConfig("crier")
	meshSvc := mesh.NewMesh(meshCfg)
	r.HandleFunc("/mesh/connect/{agentID}", mesh.HandleConnect(meshSvc))
	r.HandleFunc("/mesh/peers", mesh.HandlePeers(meshSvc)).Methods("GET")

	// Agent registry + inboxes
	var regStore registry.Store

	if cfg.Database.URL != "" {
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
		slog.Info("registry backend", "type", "memory", "hint", "set CR_DATABASE_URL for PostgreSQL")
	}

	registryHandler := registry.NewHandler(regStore)
	registryHandler.SetRequireAgentSig(cfg.RequireAgentSig)

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
	r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/ack", registryHandler.HandleAck).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/stats", registryHandler.HandleStats).Methods("GET")

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	// Periodic expired message purging
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
				regStore.PurgeExpired()
			}
		}
	}()

	// Graceful shutdown. signal.Notify is registered synchronously BEFORE
	// ListenAndServe so the handler is guaranteed installed by the time the
	// server accepts traffic — otherwise a SIGTERM arriving before the wait
	// goroutine runs (e.g. from TestServerHealth's cleanup) hits the default
	// handler and kills the process with "signal: terminated" instead of
	// shutting down gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh

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
		if closer, ok := regStore.(interface{ Close() }); ok {
			closer.Close()
		}
	}()

	slog.Info("crier starting", "port", cfg.Port, "services", "relay+mesh+registry",
		"version", buildinfo.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logServeFailure(cfg.Port, err)
		return 1
	}
	return 0
}

// logServeFailure reports a fatal ListenAndServe error. For the shared-host
// dead end (DF-CRIER-154) — the port already held by a leftover server from
// an earlier session — it turns the raw errno into the operator's next two
// commands instead: who holds the port, and how to start elsewhere. The
// build identity is repeated on the failure path so the log says WHICH
// binary failed, even when the "crier starting" line above scrolled away or
// was filtered out. The raw error stays in error= unchanged; any other
// failure keeps the one-line shape it always had.
func logServeFailure(port int, err error) {
	if !errors.Is(err, syscall.EADDRINUSE) {
		slog.Error("server failed", "error", err)
		return
	}
	slog.Error("server failed: another process already holds this port "+
		"(bind: address already in use)",
		"error", err,
		"port", port,
		"hint_holder", fmt.Sprintf("find it with: ss -tlnp | grep :%d", port),
		"hint_run_elsewhere", "start on a free port instead: -port <n> (or set CRIER_PORT=<n>)",
		"version", buildinfo.String())
}

// parseArgs parses crier's CLI flags, writing usage/error output to out.
// Return values:
//   - help: -help/--help (or -h) was requested; usage has already been
//     printed to out.
//   - showVersion: -version/--version was requested.
//   - port/dbURL: flag overrides for the env-driven config; 0/"" mean the
//     flag was not set and the environment wins.
//   - err: parse failure (unknown flag or bad value); the error message and
//     usage have already been printed to out.
func parseArgs(args []string, out io.Writer) (help, showVersion bool, port int, dbURL string, err error) {
	fs := flag.NewFlagSet("crier", flag.ContinueOnError)
	fs.SetOutput(out)
	versionFlag := fs.Bool("version", false, "print version and exit")
	portFlag := fs.Int("port", 0, "listen port (overrides CRIER_PORT)")
	dbURLFlag := fs.String("db-url", "", "PostgreSQL connection URL (overrides CR_DATABASE_URL)")
	fs.Usage = func() { printUsage(out, fs) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The flag package already printed usage. -h/-help must exit
			// 0 (the package's default ExitOnError path exits 2, which
			// violates the CR-GAP-005 acceptance criteria).
			return true, false, 0, "", nil
		}
		return false, false, 0, "", err
	}
	return false, *versionFlag, *portFlag, *dbURLFlag, nil
}

// federationName returns this relay's display name for the /fed/peers
// listing: CR_FED_NAME when set, else localhost:<port>.
func federationName(cfg config.Config) string {
	if cfg.Federation.Name != "" {
		return cfg.Federation.Name
	}
	return fmt.Sprintf("localhost:%d", cfg.Port)
}

// printUsage writes the full usage text: the flag summary plus the
// environment variables crier reads for configuration.
func printUsage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(out, "crier — pub/sub relay + mesh + registry server")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  crier [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	fs.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Configuration is read from environment variables; the")
	fmt.Fprintln(out, "-port and -db-url flags override them when set:")
	fmt.Fprintln(out, "  CRIER_PORT                  listen port (default 8767)")
	fmt.Fprintln(out, "  CR_DATABASE_URL             PostgreSQL URL (fallbacks: DATABASE_URL, CRIER_DATABASE_URL)")
	fmt.Fprintln(out, "  CR_AUTH_TOKEN               bearer token required on all requests (empty = auth disabled)")
	fmt.Fprintln(out, "  CR_REQUIRE_AGENT_SIG        require per-agent ed25519 signatures (default true)")
	fmt.Fprintln(out, "  CR_LOG_LEVEL                debug|info|warn|error (default info)")
	fmt.Fprintln(out, "  CR_LOG_FORMAT               text|json (default text)")
	fmt.Fprintln(out, "  CR_RATE_LIMIT_PER_MINUTE    relay publish rate limit (default 100)")
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
	fmt.Fprintln(out, "  DEEPSEEK_API_KEY            deepseek preset API key (env:DEEPSEEK_API_KEY ref)")
	fmt.Fprintln(out, "  CR_DATABASE_*               PostgreSQL pool tuning (MAX_CONNS, MIN_CONNS, ...)")
}
