package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/totalwindupflightsystems/crier/config"
	"github.com/totalwindupflightsystems/crier/internal/mcp"
	"github.com/totalwindupflightsystems/crier/internal/registry"
)

// version is the crier-mcp version. Overridable at build time via
// -ldflags "-X main.version=<ver>" (see the Makefile build-mcp target).
var version = "dev"

// Key sources recorded on the bridge identity.
const (
	keySourceFile      = "file"      // CRIER_AGENT_PRIVATE_KEY_FILE
	keySourceEphemeral = "ephemeral" // generated in-process for this run
)

// bridgeCapabilities are the capability tags advertised for the bridge's own
// identity when it self-registers on a remote server.
var bridgeCapabilities = []string{"mcp", "bridge"}

// bridgeIdentity is the signing identity crier-mcp holds on a remote Crier
// server: the agent id it acts as, the public half of its signing key, and
// where that key came from. Remote is false for the Postgres/in-memory
// backends, where no server-side registration is needed.
type bridgeIdentity struct {
	Remote    bool
	AgentID   string
	PublicKey ed25519.PublicKey
	KeySource string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run executes the MCP stdio server. It parses CLI flags first so
// --help/--version return immediately instead of starting the server, then
// falls through to the env-driven configuration and server startup. It
// returns a process exit code and is the testable entrypoint (main() is a
// thin wrapper), so tests can invoke it without os.Args carrying go test's
// flags.
func run(args []string) int {
	help, showVersion, err := parseArgs(args, os.Stdout)
	if err != nil {
		// The flag package already printed the error and usage to stdout.
		return 2
	}
	if help {
		return 0
	}
	if showVersion {
		fmt.Fprintf(os.Stdout, "crier-mcp %s\n", version)
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}

	store, identity, cleanup, err := initStore(cfg)
	if err != nil {
		slog.Error("initialize store", "error", err)
		return 1
	}
	defer cleanup()

	// Register the bridge's own identity before serving, so the reply leg
	// (get_messages / ask_agent polling its own inbox) works without an
	// operator registering the agent by hand (DF-CRIER-28).
	ensureBridgeIdentity(store, identity, slog.Default())

	server := mcp.NewWithOptions(store, mcp.Options{
		AgentID: os.Getenv("CRIER_AGENT_ID"),
		HTTPURL: os.Getenv("CRIER_HTTP_URL"),
		MeshURL: os.Getenv("CRIER_MESH_URL"),
	})
	if err := server.Serve(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		return 1
	}
	return 0
}

// initStore creates a Store backend: a RemoteStore against a running Crier
// server when CRIER_HTTP_URL is set, PostgreSQL if CR_DATABASE_URL is set,
// otherwise in-memory. It also returns the bridge's signing identity (only
// meaningful in remote mode) so the caller can register it on the server.
//
// In remote mode the bridge always signs its requests, which is what a
// server running with the secure default (CR_REQUIRE_AGENT_SIG=true)
// requires:
//
//   - CRIER_AGENT_PRIVATE_KEY_FILE set: that PKCS#8 PEM ed25519 key is
//     loaded; a bad or unreadable file is an explicit startup error.
//   - unset: an ephemeral ed25519 key is generated in-process so signed
//     servers work out of the box. It changes on every run, so a persistent
//     server keeps the FIRST run's registered public key — set the variable
//     for a stable identity (see the key-mismatch remedy in
//     ensureBridgeIdentity).
func initStore(cfg config.Config) (registry.Store, bridgeIdentity, func(), error) {
	if url := os.Getenv("CRIER_HTTP_URL"); url != "" {
		agentID := os.Getenv("CRIER_AGENT_ID")
		if agentID == "" {
			return nil, bridgeIdentity{}, nil, fmt.Errorf("CRIER_AGENT_ID is required when CRIER_HTTP_URL is set")
		}
		identity := bridgeIdentity{Remote: true, AgentID: agentID}
		var opts []registry.RemoteOption
		if keyFile := os.Getenv("CRIER_AGENT_PRIVATE_KEY_FILE"); keyFile != "" {
			priv, err := registry.LoadEd25519PrivateKeyFile(keyFile)
			if err != nil {
				return nil, bridgeIdentity{}, nil, fmt.Errorf("CRIER_AGENT_PRIVATE_KEY_FILE: %w", err)
			}
			opts = append(opts, registry.WithSigningKey(priv))
			identity.PublicKey = priv.Public().(ed25519.PublicKey)
			identity.KeySource = keySourceFile
		} else {
			_, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return nil, bridgeIdentity{}, nil, fmt.Errorf("generate ephemeral signing key: %w", err)
			}
			opts = append(opts, registry.WithSigningKey(priv))
			identity.PublicKey = priv.Public().(ed25519.PublicKey)
			identity.KeySource = keySourceEphemeral
			slog.Info("no CRIER_AGENT_PRIVATE_KEY_FILE set — signing with an ephemeral key generated for this run",
				"agent_id", agentID, "key_source", keySourceEphemeral,
				"note", "the key is regenerated on every run, so a persistent server keeps the first run's registered key; set CRIER_AGENT_PRIVATE_KEY_FILE for a stable identity")
		}
		return registry.NewRemoteStore(url, agentID, os.Getenv("CRIER_AUTH_TOKEN"), opts...), identity, func() {}, nil
	}
	if cfg.Database.URL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Database.ConnectTimeout)
		defer cancel()

		pgStore, err := registry.NewPostgresStoreWithPoolConfig(ctx, cfg.Database.URL, registry.PoolConfig{
			MaxConns:        cfg.Database.MaxConns,
			MinConns:        cfg.Database.MinConns,
			MaxConnLifetime: cfg.Database.MaxConnLifetime,
			MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
		})
		if err != nil {
			return nil, bridgeIdentity{}, nil, fmt.Errorf("postgres: %w", err)
		}
		return pgStore, bridgeIdentity{}, func() { pgStore.Close() }, nil
	}
	return registry.NewMemoryStore(), bridgeIdentity{}, func() {}, nil
}

// ensureBridgeIdentity registers the bridge's own agent id on the remote
// server, idempotently, and reports the outcome loudly (DF-CRIER-28: without
// this the bridge polls its own inbox under an identity the server never
// heard of, so get_messages / ask_agent fail with "agent not found" until an
// operator registers it by hand).
//
// Registration failure never aborts startup: the bridge may legitimately be
// pointed at a server that is unreachable at this instant, and a harness that
// is already running should not be killed by a bootstrap step. Every failure
// is logged at error level with its cause instead of being swallowed.
func ensureBridgeIdentity(store registry.Store, id bridgeIdentity, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if !id.Remote || id.AgentID == "" {
		return
	}
	remote, ok := store.(*registry.RemoteStore)
	if !ok {
		return
	}

	res, err := remote.EnsureRegistered(id.AgentID, id.PublicKey, bridgeCapabilities)
	if err != nil {
		logger.Error("bridge identity registration FAILED — get_messages/ask_agent will fail with \"agent not found\" until this agent is registered on the server",
			"agent_id", id.AgentID, "key_source", id.KeySource, "error", err)
		return
	}

	switch {
	case res.Created:
		logger.Info("registered bridge identity on the Crier server",
			"agent_id", id.AgentID, "key_source", id.KeySource, "capabilities", bridgeCapabilities)
	case res.KeyMatches:
		logger.Info("bridge identity already registered and the server's public key matches — no action needed",
			"agent_id", id.AgentID, "key_source", id.KeySource)
	default:
		logger.Error("bridge identity already exists on the server with a DIFFERENT public key — every signed request from this bridge will be rejected with 401",
			"agent_id", id.AgentID, "key_source", id.KeySource,
			"remedy", fmt.Sprintf("set CRIER_AGENT_PRIVATE_KEY_FILE to the private key whose public half is registered for %q, or delete the stale agent server-side (DELETE /agents/%s) and restart, or point the bridge at a fresh in-memory server", id.AgentID, id.AgentID))
	}
}

// parseArgs parses crier-mcp's CLI flags, writing usage/error output to out.
// Return values:
//   - help: -help/--help (or -h) was requested; usage has already been
//     printed to out.
//   - showVersion: -version/--version was requested.
//   - err: parse failure (unknown flag or bad value); the error message and
//     usage have already been printed to out.
func parseArgs(args []string, out io.Writer) (help, showVersion bool, err error) {
	fs := flag.NewFlagSet("crier-mcp", flag.ContinueOnError)
	fs.SetOutput(out)
	versionFlag := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() { printUsage(out, fs) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The flag package already printed usage. -h/-help must exit
			// 0 (the package's default ExitOnError path exits 2, which
			// violates the CR-GAP-017 acceptance criteria).
			return true, false, nil
		}
		return false, false, err
	}
	return false, *versionFlag, nil
}

// printUsage writes the full usage text: the flag summary plus the
// environment variables crier-mcp reads for configuration.
func printUsage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(out, "crier-mcp — MCP stdio server for Crier (pub/sub relay + mesh + registry)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  crier-mcp [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	fs.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Configuration is read from environment variables; the MCP server")
	fmt.Fprintln(out, "speaks JSON-RPC over stdin/stdout (stdio transport):")
	fmt.Fprintln(out, "  CR_DATABASE_URL             PostgreSQL URL (fallbacks: DATABASE_URL, CRIER_DATABASE_URL)")
	fmt.Fprintln(out, "  CR_AUTH_TOKEN               bearer token required on all requests (empty = auth disabled)")
	fmt.Fprintln(out, "  CR_REQUIRE_AGENT_SIG        require per-agent ed25519 signatures (default true)")
	fmt.Fprintln(out, "  CR_LOG_LEVEL                debug|info|warn|error (default info)")
	fmt.Fprintln(out, "  CR_LOG_FORMAT               text|json (default text)")
	fmt.Fprintln(out, "  CR_DATABASE_*               PostgreSQL pool tuning (MAX_CONNS, MIN_CONNS, ...)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Remote mode (set CRIER_HTTP_URL to a running Crier server):")
	fmt.Fprintln(out, "  CRIER_HTTP_URL              base URL of the Crier server (e.g. http://localhost:8767)")
	fmt.Fprintln(out, "  CRIER_AGENT_ID              agent id used on agent-owned routes (required in remote mode)")
	fmt.Fprintln(out, "  CRIER_AUTH_TOKEN            shared bearer token when the server runs with CR_AUTH_TOKEN")
	fmt.Fprintln(out, "  CRIER_AGENT_PRIVATE_KEY_FILE  optional PKCS#8 PEM ed25519 private key (openssl genpkey")
	fmt.Fprintln(out, "                              -algorithm ED25519). Enables per-agent request signing")
	fmt.Fprintln(out, "                              (X-Agent-Ts/X-Agent-Sig on every request) for servers with")
	fmt.Fprintln(out, "                              CR_REQUIRE_AGENT_SIG=true (the secure default). Key material")
	fmt.Fprintln(out, "                              is never logged.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Bridge registration (remote mode, automatic — no operator step):")
	fmt.Fprintln(out, "  On startup crier-mcp registers CRIER_AGENT_ID on the server, idempotently:")
	fmt.Fprintln(out, "  an existing registration is left untouched, so re-running is a no-op.")
	fmt.Fprintln(out, "  Without CRIER_AGENT_PRIVATE_KEY_FILE an ephemeral ed25519 key is generated")
	fmt.Fprintln(out, "  in-process, so requests are signed out of the box against the secure default.")
	fmt.Fprintln(out, "  That key changes on every run: a persistent server keeps the FIRST run's")
	fmt.Fprintln(out, "  registered public key, and later runs log a key-mismatch ERROR because every")
	fmt.Fprintln(out, "  signed request would be rejected with 401. Remedies then: set")
	fmt.Fprintln(out, "  CRIER_AGENT_PRIVATE_KEY_FILE to the private key whose public half is registered")
	fmt.Fprintln(out, "  for CRIER_AGENT_ID, or delete the stale agent server-side (DELETE /agents/{id})")
	fmt.Fprintln(out, "  and restart, or point the bridge at a fresh in-memory server. Set the variable")
	fmt.Fprintln(out, "  for any long-lived bridge. Servers that disable signature enforcement")
	fmt.Fprintln(out, "  (CR_REQUIRE_AGENT_SIG=false) ignore the key, and registration still works.")
	fmt.Fprintln(out, "  A registration failure (server unreachable, signing disabled, ...) is logged at")
	fmt.Fprintln(out, "  error level and never aborts startup.")
}
