// sessionroutes.go — store selection for the session API surface (CR-CHAT-019).
//
// The six routes are a CLIENT of the shipped primitives: the session data
// model and its ordering authority come from internal/session, and every
// fan-out write goes through the same registry.Store.Deliver the inbox routes
// use. This file only opens the store (CR_SESSION_BACKEND) and builds the
// handler; the always-on routes are registered in main.go beside the other
// always-on surfaces, so the router-path count the README publishes measures
// them (the OPT-IN dagger surface, by contrast, registers from its own file so
// an off-by-default feature does not inflate that count).
package main

import (
	"context"
	"fmt"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
)

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
func openSessionAPI(
	cfg config.Config,
	registryStore registry.Store,
	namespaces *namespace.Registry,
	perms *permissions.Checker,
	grantEvents func(context.Context, string) ([]session.PermissionsGrantView, error),
) (session.Store, *session.Handler, error) {
	sessStore, err := session.OpenStore(context.Background(), cfg.ResolvedSessionBackend(), session.StoreOptions{
		SQLitePath:  cfg.Session.SQLitePath,
		LogRoot:     cfg.Session.LogRoot,
		DatabaseURL: cfg.Database.URL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("session api: %w", err)
	}

	// Named groups (CR-CHAT-013, specs/CHAT-ADDRESSING.md §1.4): the roster
	// log lives beside the session store regardless of CR_SESSION_BACKEND, so
	// the curated rosters survive every backend selection. A store that
	// cannot open is a boot error rather than a surface that silently serves
	// no groups — the same rule openSessionAPI applies to the session store.
	groupStore, err := session.NewJSONLGroupStore(cfg.Session.GroupRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("group api: %w", err)
	}

	h := session.NewHTTPHandler(sessStore, session.HTTPOptions{
		Deliverer:   registryStore,
		Agents:      registryStore,
		Namespaces:  namespaces,
		Permissions: perms,
		Groups:      groupStore,
		GrantEvents: grantEvents,
	})
	return sessStore, h, nil
}
