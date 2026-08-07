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

	"github.com/gorilla/mux"
	"github.com/totalwindupflightsystems/crier/config"
	"github.com/totalwindupflightsystems/crier/internal/mesh"
	"github.com/totalwindupflightsystems/crier/internal/middleware"
	"github.com/totalwindupflightsystems/crier/internal/registry"
	"github.com/totalwindupflightsystems/crier/internal/relay"
)

// version is the crier server version. Overridable at build time via
// -ldflags "-X main.version=<ver>" (see the Makefile build target).
var version = "dev"

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
		fmt.Fprintf(os.Stdout, "crier %s\n", version)
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

	// Middleware
	r.Use(middleware.Auth(cfg.AuthToken))
	r.Use(middleware.Recovery)
	r.Use(middleware.Logging)

	// Health check
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}).Methods("GET")

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
	r.HandleFunc("/agents", registryHandler.HandleRegister).Methods("POST")
	r.HandleFunc("/agents", registryHandler.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", registryHandler.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", registryHandler.HandleUnregister).Methods("DELETE")
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
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("http shutdown", "error", err)
		}
		if closer, ok := regStore.(interface{ Close() }); ok {
			closer.Close()
		}
	}()

	slog.Info("crier starting", "port", cfg.Port, "services", "relay+mesh+registry")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		return 1
	}
	return 0
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
	fmt.Fprintln(out, "  CR_DATABASE_*               PostgreSQL pool tuning (MAX_CONNS, MIN_CONNS, ...)")
}
