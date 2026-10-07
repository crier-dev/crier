// permissions.go — building the delivery ACL at boot (CR-CHAT-003,
// specs/CHAT-PERMISSIONS.md §6).
//
// One resolution point, so a deployment cannot arm the ACL on the registry
// lane without knowing where its store lives. The store is the JSONL
// append-only log: principals, bindings, grants and agent-class records in one
// file, folded by keep-LAST per id.
package main

import (
	"context"
	"fmt"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/session"
)

// buildPermissions resolves the delivery ACL from configuration. It returns
// (nil, nil, nil) when the ACL is not enabled — that is the shipped posture,
// and it is NOT the same statement as "no grants": no checker is wired at all,
// so the delivery path is byte-identical to a build without this feature (§6.4
// rule 2).
//
// An enabled ACL whose store directory cannot be created is an ERROR, never a
// silent downgrade: an operator who turned the ACL ON and got an unwritable
// store must not be served the trust-by-reach posture while believing the ACL
// is armed.
//
// The third return is the CR-CHAT-021 audit bridge: a READ-ONLY function that
// renders the store's grant records (live and tombstoned) as the grant views
// the session audit trail and permission matrix overlay. It is nil exactly
// when the checker is — a deployment with no ACL has no permission events to
// show, and the audit read answers with session events only. It never writes.
func buildPermissions(cfg config.Config) (*permissions.Checker, func(context.Context, string) ([]session.PermissionsGrantView, error), error) {
	if !cfg.Permissions.Enabled {
		return nil, nil, nil
	}
	store, err := permissions.NewJSONLStore(cfg.Permissions.StoreDir)
	if err != nil {
		return nil, nil, fmt.Errorf("delivery ACL store %q: %w", cfg.Permissions.StoreDir, err)
	}
	return permissions.NewChecker(store), grantEventsReader(store), nil
}

// grantEventsReader adapts a permissions JSONL store to the session audit
// surface's grant view. Only grant records are read; principals, bindings and
// agent-class records are the ACL's own surface and are not re-rendered here.
func grantEventsReader(store *permissions.JSONLStore) func(context.Context, string) ([]session.PermissionsGrantView, error) {
	return func(ctx context.Context, sessionID string) ([]session.PermissionsGrantView, error) {
		recs, err := store.Records(ctx)
		if err != nil {
			return nil, err
		}
		var out []session.PermissionsGrantView
		for _, rec := range recs {
			if rec == nil || rec.Type != permissions.RecordGrant || rec.Grant == nil {
				continue
			}
			g := rec.Grant
			// The session audit reads grants whose subject is THIS session
			// (§6.1's session subject). A grant on any other subject belongs
			// to that subject's own audit story, not this session's.
			if g.Subject.Type != permissions.SubjectSession || g.Subject.Ref != sessionID {
				continue
			}
			actions := make([]string, 0, len(g.Actions))
			for _, a := range g.Actions {
				actions = append(actions, string(a))
			}
			view := session.PermissionsGrantView{
				ID:        g.ID,
				Principal: g.Principal,
				Actions:   actions,
				GrantedBy: g.GrantedBy,
				GrantedAt: g.GrantedAt,
			}
			if g.RevokedAt != nil {
				revokedAt := *g.RevokedAt
				view.RevokedAt = &revokedAt
				view.RevokedBy = g.RevokedBy
			}
			out = append(out, view)
		}
		return out, nil
	}
}
