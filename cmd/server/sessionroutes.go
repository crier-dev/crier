// sessionroutes.go — store selection for the session API surface (CR-CHAT-019).
//
// The six routes are a CLIENT of the shipped primitives: the session data
// model and its ordering authority come from internal/session, and every
// fan-out enters the same guarded registry dispatcher the inbox routes use.
// This file only opens the store (CR_SESSION_BACKEND) and builds the
// handler; the always-on routes are registered in main.go beside the other
// always-on surfaces, so the router-path count the README publishes measures
// them (the OPT-IN dagger surface, by contrast, registers from its own file so
// an off-by-default feature does not inflate that count).
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/reads"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
)

// openReadCursors opens the read-cursor store that belongs to the SELECTED
// session backend (CRIER-READS-1, specs/COURIER-READS.md §4).
//
// The cursor is session state, so it follows the session backend rather than
// introducing a fourth storage choice: a JSONL deployment keeps its cursors at
// `<CR_SESSION_LOG_ROOT>/reads/<session_id>.jsonl` beside the transcripts they
// describe, and a SQLite/PostgreSQL deployment keeps them in the
// chat_read_cursors table. A store that cannot open is a boot error, the same
// rule the session store itself follows — an operator must never get a server
// that silently answers "nobody has read anything".
func openReadCursors(ctx context.Context, cfg config.Config) (reads.Store, error) {
	switch cfg.ResolvedSessionBackend() {
	case config.SessionBackendSQLite:
		path := strings.TrimSpace(cfg.Session.SQLitePath)
		if path == "" {
			path = config.DefaultSQLitePath
		}
		return reads.NewSQLiteStore(ctx, path)
	case config.SessionBackendPostgres:
		return reads.NewPostgresStore(ctx, cfg.Database.URL)
	default:
		root := strings.TrimSpace(cfg.Session.LogRoot)
		if root == "" {
			root = config.DefaultSessionLogRoot
		}
		return reads.NewJSONLStore(root)
	}
}

// openSessionAPI opens the chat-session store named by CR_SESSION_BACKEND and
// builds the session API handler. It returns the opened store so the caller can
// close it on shutdown.
//
// It is realm-scoped on arrival (CHAT-INTERFACE.md §4 row 38): the store
// records the session's namespace, reads resolve the same X-Crier-Namespace
// convention the relay and the registry use, and a fan-out refuses a target
// registered in another realm.
//
// A store that cannot open is a boot error, never a silent absence of the
// surface — an operator who selected CR_SESSION_BACKEND=sqlite must not get a
// server that quietly serves no sessions.
// groupStoreForSessionStore selects the named-group backend that belongs to
// the already-open session backend. PostgreSQL and SQLite expose Groups over
// their own database handle; JSONL remains the file-backed pairing for the
// JSONL session store.
func groupStoreForSessionStore(sessStore session.Repository, groupRoot string) (session.GroupStore, error) {
	switch st := sessStore.(type) {
	case *session.PostgresStore:
		return st.Groups(), nil
	case *session.SQLStore:
		return st.Groups(), nil
	case *session.JSONLStore:
		return session.NewJSONLGroupStore(groupRoot)
	default:
		return nil, fmt.Errorf("session backend %T has no matching group backend", sessStore)
	}
}

func openSessionAPI(
	cfg config.Config,
	registryStore registry.Store,
	dispatcher *registry.Handler,
	namespaces *namespace.Registry,
	perms *permissions.Checker,
	grantEvents func(context.Context, string) ([]session.PermissionsGrantView, error),
) (session.Repository, *session.Handler, *reads.HTTPHandler, error) {
	sessStore, err := session.OpenStore(context.Background(), cfg.ResolvedSessionBackend(), session.StoreOptions{
		SQLitePath:  cfg.Session.SQLitePath,
		LogRoot:     cfg.Session.LogRoot,
		DatabaseURL: cfg.Database.URL,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("session api: %w", err)
	}

	// Named groups (CR-CHAT-022): the roster persists in the SELECTED session
	// backend — the SQL group store over the same database when the backend is
	// SQLite or PostgreSQL, the JSONL roster log otherwise. The tables are
	// part of the session schema, so an existing database reaches them with no
	// migration step. A store that cannot open is a boot error rather than a
	// surface that silently serves no groups — the same rule openSessionAPI
	// applies to the session store.
	groupStore, err := groupStoreForSessionStore(sessStore, cfg.Session.GroupRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("group api: %w", err)
	}

	// Keep this selection tied to the session store's concrete backend. In
	// particular, PostgresStore has its own Groups implementation over the same
	// pool; falling through to JSONL would silently split session-scoped state.

	h := session.NewHTTPHandler(sessStore, session.HTTPOptions{
		Deliverer:             registry.NewSessionDeliverer(dispatcher),
		RequireAgentSignature: cfg.RequireAgentSig,
		AuthorizeSender:       dispatcher.AuthorizePublisher,
		DeliveryTimeoutMs:     cfg.Session.DeliveryTimeoutMs,
		Agents:                registryStore,
		Namespaces:            namespaces,
		Permissions:           perms,
		Groups:                groupStore,
		GrantEvents:           grantEvents,
		// The delivery LOCATION (CR-CHAT-029): the same name /fed/peers
		// announces, so an agent reading its location and an operator
		// reading the peers listing see the same identity.
		InstanceName: federationName(cfg),
	})

	// CRIER-READS-1: the read-state surface is built here, beside the session
	// handler it shares its visibility rule with, and its three routes are
	// registered in main.go with the other always-on session routes. The
	// handler holds session.Handler as its SessionAccess, so the realm wall and
	// the private-session rule are decided in exactly one place.
	cursorStore, err := openReadCursors(context.Background(), cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read cursors: %w", err)
	}
	readHandler := reads.NewHTTPHandler(
		&reads.Service{Repo: sessStore, Cursors: cursorStore},
		h, &reads.Authorizer{Permissions: perms},
	)
	return sessStore, h, readHandler, nil
}
