// courier.go — the crier-mcp side of the unread nudge (CRIER-MCP-NUDGE §3, §4).
//
// The nudge reports COUNTS: the agent-inbox queue depth from the same store the
// inbox tools use, and the per-session unread state from the session transcript
// plus the read cursors. The bridge therefore needs the session backend, and
// this file opens the one the deployment configured — the SAME selection
// cmd/server makes (CR_SESSION_BACKEND, CR_SESSION_LOG_ROOT,
// CR_SESSION_SQLITE_PATH), so both processes read one store.
//
// Wiring is OPT-IN in the sense that matters: a process with no session
// configuration at all opens nothing and creates nothing, and the summary
// reports the agent-inbox number alone. A deployment that runs the session API
// sets CR_SESSION_BACKEND (or a path), and then the session counts appear —
// including on a bridge that runs beside the server on the same host. A bridge
// on ANOTHER host still sees only its own local files; the honest remedy is to
// point it at the same backend, not to invent numbers.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/mcp"
	"github.com/crier-dev/crier/internal/reads"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
)

// sessionConfigured reports whether the deployment has said anything about the
// session backend. ResolvedSessionBackend defaults to JSONL, so opening the
// "resolved" store unconditionally would create a crier-sessions/ directory in
// whatever CWD the bridge happens to start in — a side effect this row must
// not introduce for a deployment that never used sessions.
func sessionConfigured(cfg config.Config) bool {
	return strings.TrimSpace(cfg.Session.Backend) != "" ||
		strings.TrimSpace(cfg.Session.LogRoot) != "" ||
		strings.TrimSpace(cfg.Session.SQLitePath) != ""
}

// openCrier wires the nudge's read side from config. It never fails startup:
// a store that cannot open is logged and the nudge falls back to the
// agent-inbox count, because a nudge must never be the reason an agent's bridge
// will not start. The returned cleanup closes what was opened.
func openCrier(cfg config.Config, store registry.Store, logger *slog.Logger) (mcp.CrierSource, func()) {
	if logger == nil {
		logger = slog.Default()
	}
	if !sessionConfigured(cfg) {
		logger.Info("Crier nudge: no session backend configured in this process — unread counts cover the agent inbox only",
			"hint", "set CR_SESSION_BACKEND (and CR_SESSION_LOG_ROOT / CR_SESSION_SQLITE_PATH) to the server's session store")
		return nil, func() {}
	}

	ctx := context.Background()
	backend := cfg.ResolvedSessionBackend()
	repo, err := session.OpenStore(ctx, backend, session.StoreOptions{
		SQLitePath:  cfg.Session.SQLitePath,
		LogRoot:     cfg.Session.LogRoot,
		DatabaseURL: cfg.Database.URL,
	})
	if err != nil {
		logger.Warn("Crier nudge: session store could not be opened — unread counts cover the agent inbox only",
			"backend", backend, "error", err)
		return nil, func() {}
	}
	cursors, closeCursors, err := openReadCursors(cfg, backend)
	if err != nil {
		closeStore(repo)
		logger.Warn("Crier nudge: read cursors could not be opened — unread counts cover the agent inbox only",
			"backend", backend, "error", err)
		return nil, func() {}
	}
	logger.Info("Crier nudge: session unread wired", "backend", backend)
	return &mcp.Crier{Inbox: store, Sessions: repo, Reads: &reads.Service{Repo: repo, Cursors: cursors}},
		func() {
			closeCursors()
			closeStore(repo)
		}
}

// openReadCursors mirrors cmd/server's selection: the cursor store follows the
// session backend, never introduces a fourth storage choice.
func openReadCursors(cfg config.Config, backend string) (reads.Store, func(), error) {
	switch backend {
	case config.SessionBackendSQLite:
		path := strings.TrimSpace(cfg.Session.SQLitePath)
		if path == "" {
			path = config.DefaultSQLitePath
		}
		store, err := reads.NewSQLiteStore(context.Background(), path)
		if err != nil {
			return nil, func() {}, fmt.Errorf("sqlite cursor store: %w", err)
		}
		return store, func() { _ = store.Close() }, nil
	case config.SessionBackendPostgres:
		store, err := reads.NewPostgresStore(context.Background(), cfg.Database.URL)
		if err != nil {
			return nil, func() {}, fmt.Errorf("postgres cursor store: %w", err)
		}
		return store, func() { _ = store.Close() }, nil
	default:
		root := strings.TrimSpace(cfg.Session.LogRoot)
		if root == "" {
			root = config.DefaultSessionLogRoot
		}
		store, err := reads.NewJSONLStore(root)
		if err != nil {
			return nil, func() {}, fmt.Errorf("jsonl cursor store: %w", err)
		}
		return store, func() {}, nil
	}
}

// closeStore closes a session repository when the concrete backend owns a
// handle. The JSONL store has nothing to close.
func closeStore(repo session.Repository) {
	if closer, ok := repo.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}
