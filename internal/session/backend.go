package session

import (
	"context"
	"fmt"
	"strings"
)

// The CR_SESSION_BACKEND vocabulary (CR-CHAT-006). These are the values the
// config layer validates; OpenStore selects the adapter from one of them.
const (
	// BackendAuto is the unset selection. It is resolved by the caller (the
	// config layer) — never by OpenStore — because "auto" is a deployment
	// policy (postgres when a database URL is configured, jsonl otherwise),
	// not an engine.
	BackendAuto = ""
	// BackendSQLite is the pure-Go SQLite view: no PostgreSQL service.
	BackendSQLite = "sqlite"
	// BackendPostgres is the server-backed PostgreSQL view.
	BackendPostgres = "postgres"
	// BackendJSONL is the JSONL ordered append log and transport form.
	BackendJSONL = "jsonl"
)

// StoreOptions carries the per-backend settings OpenStore needs. Every field
// is only read by the backend that names it, so an unset field is not an error
// for a backend that does not use it.
type StoreOptions struct {
	// SQLitePath is the SQLite database file (BackendSQLite).
	SQLitePath string
	// LogRoot is the JSONL log root directory (BackendJSONL).
	LogRoot string
	// DatabaseURL is the PostgreSQL connection string (BackendPostgres).
	DatabaseURL string
}

// OpenStore opens the chat-session store the selected backend names. It is the
// ONE place a deployment turns CR_SESSION_BACKEND into a Store, so the choice
// between a local SQLite view, a server-backed PostgreSQL view and the JSONL
// log is a config value rather than a code branch at every call site.
//
// backend must be a resolved engine — BackendSQLite, BackendPostgres or
// BackendJSONL. BackendAuto is refused: "auto" is a config-layer policy, and a
// store that guessed the engine would hide the resolution.
func OpenStore(ctx context.Context, backend string, opts StoreOptions) (Store, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case BackendSQLite:
		return NewSQLiteStore(opts.SQLitePath)
	case BackendPostgres:
		if strings.TrimSpace(opts.DatabaseURL) == "" {
			return nil, fmt.Errorf("session store: backend %q requires a database URL (CR_DATABASE_URL)", BackendPostgres)
		}
		return NewPostgresStore(ctx, opts.DatabaseURL)
	case BackendJSONL:
		return NewJSONLStore(opts.LogRoot)
	case BackendAuto:
		return nil, fmt.Errorf("session store: backend is empty; resolve CR_SESSION_BACKEND to one of %q, %q or %q",
			BackendSQLite, BackendPostgres, BackendJSONL)
	default:
		return nil, fmt.Errorf("session store: unknown backend %q (want %q, %q or %q)",
			backend, BackendSQLite, BackendPostgres, BackendJSONL)
	}
}
