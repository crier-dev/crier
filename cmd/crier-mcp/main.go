package main

import (
	"context"
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

	store, cleanup, err := initStore(cfg)
	if err != nil {
		slog.Error("initialize store", "error", err)
		return 1
	}
	defer cleanup()

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
// otherwise in-memory.
//
// In remote mode, CRIER_AGENT_PRIVATE_KEY_FILE (optional) loads a PKCS#8 PEM
// ed25519 private key and enables per-agent request signing for servers
// running with CR_REQUIRE_AGENT_SIG=true (the secure default). A bad or
// missing key file is an explicit startup error; without the variable the
// bridge stays unsigned, which remains valid against servers that disable
// per-agent signatures.
func initStore(cfg config.Config) (registry.Store, func(), error) {
	if url := os.Getenv("CRIER_HTTP_URL"); url != "" {
		agentID := os.Getenv("CRIER_AGENT_ID")
		if agentID == "" {
			return nil, nil, fmt.Errorf("CRIER_AGENT_ID is required when CRIER_HTTP_URL is set")
		}
		var opts []registry.RemoteOption
		if keyFile := os.Getenv("CRIER_AGENT_PRIVATE_KEY_FILE"); keyFile != "" {
			priv, err := registry.LoadEd25519PrivateKeyFile(keyFile)
			if err != nil {
				return nil, nil, fmt.Errorf("CRIER_AGENT_PRIVATE_KEY_FILE: %w", err)
			}
			opts = append(opts, registry.WithSigningKey(priv))
		}
		return registry.NewRemoteStore(url, agentID, os.Getenv("CRIER_AUTH_TOKEN"), opts...), func() {}, nil
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
			return nil, nil, fmt.Errorf("postgres: %w", err)
		}
		return pgStore, func() { pgStore.Close() }, nil
	}
	return registry.NewMemoryStore(), func() {}, nil
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
	fmt.Fprintln(out, "                              CR_REQUIRE_AGENT_SIG=true. Omit it only when the server sets")
	fmt.Fprintln(out, "                              CR_REQUIRE_AGENT_SIG=false. The public half of the key must be")
	fmt.Fprintln(out, "                              registered for CRIER_AGENT_ID. Key material is never logged.")
}
