// daggerctl.go — wiring for the OPT-IN dagger control surface (CR-CHAT-033).
//
// The surface is a CLIENT of an executor crier does not own (decision D18):
// every control verb is one request to the dagger HTTP/JSON endpoint named by
// CR_DAGGER_URL, and crier holds only the run records it creates. With
// CR_DAGGER_URL unset this file registers nothing, so a deployment that does
// not control DAGs serves exactly the surface it served before the feature
// existed.
//
// The routes live here rather than in main.go for the same reason the A2A
// option's do: an opt-in surface must not inflate the always-on route count
// main.go publishes (README's "router paths registered in cmd/server/main.go").
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/registry"
)

// The two variables the startup store check's error message points at
// (DF-CRIER-303): the one that selects the directory, and the one whose being
// set is why a directory is required at all.
const (
	daggerStoreEnv = "CR_DAGGER_STORE_DIR"
	daggerURLEnv   = "CR_DAGGER_URL"
)

// ensureDaggerStoreDir proves the dagger run-record directory is USABLE before
// any store is opened on it (DF-CRIER-303).
//
// Why this is a check of its own, and not just the store's own MkdirAll: a
// creatability test is not a writability test. os.MkdirAll returns nil for any
// path that already IS a directory, whatever that directory's mode is — it only
// consults permission bits on the components it has to create. So the shipped
// boot failed in two different, both unhelpful, ways on a non-root box:
//
//   - a missing store under an unwritable parent failed with
//     `mkdir /var/lib/crier: permission denied` — the first ANCESTOR that could
//     not be created, not the configured directory, and the variable that
//     selects it was nowhere in the message;
//   - a store directory that already existed but was read-only booted clean and
//     only failed at the first Append, long after the process reported success.
//
// Every failure here names the variable, the resolved path and the uid, and
// says what to change, so a deploy can be fixed without reading this source.
//
// There is deliberately NO fallback to a temporary directory: a deployment that
// armed the dagger surface and silently recorded its runs somewhere else would
// lose every run record on the next restart while reporting success — the same
// reasoning the delivery ACL's store carries (cmd/server/permissions.go). The
// fix belongs in the configuration, and the error says which knob to turn.
func ensureDaggerStoreDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		// Unreachable from the boot path (config.go defaults the value
		// whenever CR_DAGGER_URL is set); kept so the check is total.
		return fmt.Errorf("%s is empty", daggerStoreEnv)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return daggerStoreUnusable(dir, "cannot create the directory", err)
	}
	// The directory exists. Prove the process can actually write in it: this is
	// exactly the case MkdirAll cannot see (an existing read-only directory).
	probe, err := os.CreateTemp(dir, ".crier-dagger-writable-*")
	if err != nil {
		return daggerStoreUnusable(dir, "cannot create a file in the directory", err)
	}
	name := probe.Name()
	if cerr := probe.Close(); cerr != nil {
		_ = os.Remove(name)
		return daggerStoreUnusable(dir, "cannot write a file in the directory", cerr)
	}
	if err := os.Remove(name); err != nil {
		// The write landed but the cleanup did not: say so rather than leaving
		// an unexplained file in an operator's store directory.
		return fmt.Errorf("%s %q is writable, but the write probe %q could not be removed: %w",
			daggerStoreEnv, dir, name, err)
	}
	return nil
}

// daggerStoreUnusable renders the one boot error an operator must be able to act
// on: what was tried, why it failed, and the ways out. The OS cause stays
// attached (via %w) so a caller can classify the failure —
// errors.Is(err, fs.ErrPermission), syscall.ENOTDIR, … — without parsing the
// prose.
func daggerStoreUnusable(dir, what string, cause error) error {
	return fmt.Errorf("%s %q is not writable by uid %d: %s: %w; the dagger surface is armed (%s is set) so a run-record store is required; set %s to a directory this user can write, or run as a user that can",
		daggerStoreEnv, dir, os.Geteuid(), what, cause, daggerURLEnv, daggerStoreEnv)
}

// registerDaggerRoutes builds the control service and registers its routes.
// It returns (nil, nil) when the surface is switched off — no bridge, no store,
// no routes — and never a half-wired surface: a bad CR_DAGGER_URL or an
// unusable store directory is an error the caller turns into a boot failure.
//
// The routes:
//
//	POST /dagger/runs                   create a prompt-driven run
//	GET  /dagger/runs/{id}              read a run's record
//	POST /dagger/runs/{id}/cancel       cancel a run
//	POST /dagger/runs/{id}/resume       resume a run from its checkpoint
//	POST /dagger/runs/{id}/rewind       rewind a run to a node
//	POST /dagger/skills/{skill}/run     run a registered skill
//	POST /dagger/wait                   deliver to an agent and wait (CR-CHAT-034)
//	POST /dagger/waits/{key}/resolve    record a reply on a wait (CR-CHAT-034)
//	GET  /dagger/waits/{key}            read a wait's state (CR-CHAT-034)
//
// Everything is additive: no existing route, body or auth requirement moves,
// and the terminal outcome travels the SHIPPED inbox path (registry Deliver)
// rather than a side channel.
func registerDaggerRoutes(r *mux.Router, cfg config.DaggerConfig, store registry.Store) (*daggerctl.Service, *registry.Handler, error) {
	if cfg.URL == "" {
		return nil, nil, nil
	}

	// DF-CRIER-303: the run-record store is settled FIRST — before the bridge,
	// before any store is opened — so an unusable directory reports itself
	// (variable, resolved path, uid) instead of surfacing as a bare
	// `mkdir <first-failing-ancestor>: permission denied` that names neither
	// the configured path nor the variable that selects it.
	if err := ensureDaggerStoreDir(cfg.StoreDir); err != nil {
		return nil, nil, fmt.Errorf("dagger control: %w", err)
	}

	opts := []daggerctl.HTTPBridgeOption{daggerctl.WithBearerToken(cfg.Token)}
	if cfg.Timeout > 0 {
		opts = append(opts, daggerctl.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}))
	}
	bridge, err := daggerctl.NewHTTPBridge(cfg.URL, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dagger control: %w", err)
	}

	runStore, err := daggerctl.NewJSONLStore(cfg.StoreDir)
	if err != nil {
		return nil, nil, fmt.Errorf("dagger control: %w", err)
	}

	svc := daggerctl.NewService(bridge, runStore, daggerctl.InboxDeliverer(store))
	svc.SetWatchInterval(cfg.PollInterval)

	// Delivery waits (CR-CHAT-034): the waiter registry journals beside the
	// run records so an idempotency key recorded before a crash still refuses
	// a second delivery after a restart. The waits deliver through the SAME
	// store the notifications use. A registry that cannot open fails the boot
	// — a wait surface that silently lost its journal would be a second
	// delivery waiting to happen.
	waits, err := daggerctl.NewWaiterRegistry(cfg.StoreDir)
	if err != nil {
		return nil, nil, fmt.Errorf("dagger control: %w", err)
	}
	svc.SetWaitRegistry(waits)
	svc.SetInboxStore(store)

	// Named remote execution targets (CR-CHAT-035): the local bridge above is
	// the default target, and each CR_DAGGER_TARGET_<NAME>=<url> adds a named
	// remote one. A create may name either; the resolved name is recorded on
	// the run and every later observation goes to that recorded target. An
	// entry whose URL is not an absolute http(s) URL fails the boot, exactly
	// as a bad CR_DAGGER_URL does.
	targets, err := daggerctl.NewTargetTable(cfg.URL, cfg.Targets)
	if err != nil {
		return nil, nil, fmt.Errorf("dagger control: %w", err)
	}
	svc.SetTargetTable(targets)

	h := daggerctl.NewHandler(svc)
	r.HandleFunc("/dagger/runs", h.HandleCreateRun).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}", h.HandleGetRun).Methods(http.MethodGet)
	r.HandleFunc("/dagger/runs/{id}/cancel", h.HandleCancel).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/resume", h.HandleResume).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/rewind", h.HandleRewind).Methods(http.MethodPost)
	r.HandleFunc("/dagger/skills/{skill}/run", h.HandleRunSkill).Methods(http.MethodPost)
	// Delivery waits (CR-CHAT-034): deliver-and-wait, resolve, and status.
	r.HandleFunc("/dagger/wait", h.HandleDeliverAndWait).Methods(http.MethodPost)
	r.HandleFunc("/dagger/waits/{key}/resolve", h.HandleResolveWait).Methods(http.MethodPost)
	r.HandleFunc("/dagger/waits/{key}", h.HandleWaitStatus).Methods(http.MethodGet)
	return svc, nil, nil
}
