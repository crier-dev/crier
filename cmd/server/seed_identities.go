package main

import (
	"log/slog"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/registry"
)

// applyRegistrySeedIdentities is the boot-time half of
// CR_REGISTRY_SEED_IDENTITIES: it turns the operator's declaration into rows in
// a store that starts empty, BEFORE composeRegistryDelivery validates the
// allowlist against that store.
//
// It is a DECLARATION, not auto-registration. The delivery guard is unchanged:
// every identity named by an allowlist pair must still resolve to a real row
// with a valid Ed25519 public key, and a pair whose identity is neither in the
// store nor in this declaration still fails the boot with the identical
// "service identity %q is not pre-provisioned" error. Nothing here reads the
// allowlist, so a declared identity grants no delivery authority on its own —
// only the allowlist does, exactly as before.
//
// The returned error is the boot refusal; the caller logs and exits 1.
func applyRegistrySeedIdentities(cfg config.Config, store registry.Store) (registry.Store, error) {
	ids, err := registry.ParseSeedIdentities(cfg.RegistrySeedIdentities)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return store, nil
	}
	if err := registry.ApplySeedIdentities(store, ids); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, id.ID)
	}
	// Ids are public identifiers; no key material is logged.
	slog.Info("registry identities seeded from CR_REGISTRY_SEED_IDENTITIES",
		"count", len(ids), "ids", names,
		"detail", "operator-declared identities provisioned into the in-memory store before service-delivery validation; delivery authority still comes only from CR_SERVICE_DELIVERY_ALLOWLIST")
	return store, nil
}
