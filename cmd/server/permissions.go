// permissions.go — building the delivery ACL at boot (CR-CHAT-003,
// specs/CHAT-PERMISSIONS.md §6).
//
// One resolution point, so a deployment cannot arm the ACL on the registry
// lane without knowing where its store lives. The store is the JSONL
// append-only log: principals, bindings, grants and agent-class records in one
// file, folded by keep-LAST per id.
package main

import (
	"fmt"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/permissions"
)

// buildPermissions resolves the delivery ACL from configuration. It returns
// (nil, nil) when the ACL is not enabled — that is the shipped posture, and it
// is NOT the same statement as "no grants": no checker is wired at all, so the
// delivery path is byte-identical to a build without this feature (§6.4
// rule 2).
//
// An enabled ACL whose store directory cannot be created is an ERROR, never a
// silent downgrade: an operator who turned the ACL ON and got an unwritable
// store must not be served the trust-by-reach posture while believing the ACL
// is armed.
func buildPermissions(cfg config.Config) (*permissions.Checker, error) {
	if !cfg.Permissions.Enabled {
		return nil, nil
	}
	store, err := permissions.NewJSONLStore(cfg.Permissions.StoreDir)
	if err != nil {
		return nil, fmt.Errorf("delivery ACL store %q: %w", cfg.Permissions.StoreDir, err)
	}
	return permissions.NewChecker(store), nil
}
