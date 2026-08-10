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

	server := mcp.New(store)
	if err := server.Serve(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		return 1
	}
	return 0
}

// initStore creates a Store backend: PostgreSQL if CR_DATABASE_URL is set, otherwise in-memory.
func initStore(cfg config.Config) (registry.Store, func(), error) {
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
}
