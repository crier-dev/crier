package registry

import (
	"fmt"
	"net/http"
	"time"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/gorilla/mux"
)

// principalReadView is an explicit allowlist, NOT an embedded stored Principal:
// adding credential fields to the storage schema must never expand this wire.
type principalReadView struct {
	ID               string                      `json:"id"`
	Kind             string                      `json:"kind"`
	CreatedAt        time.Time                   `json:"created_at"`
	TokenFingerprint string                      `json:"token_fingerprint,omitempty"`
	DisplayName      string                      `json:"display_name,omitempty"`
	Namespace        string                      `json:"namespace,omitempty"`
	Role             permissions.Role            `json:"role,omitempty"`
	Status           permissions.PrincipalStatus `json:"status,omitempty"`
	Remote           bool                        `json:"remote,omitempty"`
	RemoteInstance   string                      `json:"remote_instance,omitempty"`
	RemoteRef        string                      `json:"remote_ref,omitempty"`
	LastLoginAt      *time.Time                  `json:"last_login_at,omitempty"`
}

type permissionList[T any] struct {
	Items []T `json:"items"`
	Count int `json:"count"`
}

// managementSnapshot reuses the write surface's sole admin gate and the delivery
// ACL's fold. Snapshot only opens an existing log for reading: no new store,
// directory creation, append, migration or cache is performed by these reads.
func (h *Handler) managementSnapshot(w http.ResponseWriter, r *http.Request) *permissions.Snapshot {
	if !h.managementAuthorized(r) {
		refuseManagement(w, h.permissionsStore != nil)
		return nil
	}
	snap, err := h.permissionsStore.Snapshot(r.Context())
	if err != nil || snap == nil {
		// A malformed record may carry credential material in its diagnostic.
		// Do not echo or log the store error on a read surface.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failure"})
		return nil
	}
	return snap
}

// HandleListPrincipals serves the current principal inventory without secrets.
func (h *Handler) HandleListPrincipals(w http.ResponseWriter, r *http.Request) {
	snap := h.managementSnapshot(w, r)
	if snap == nil {
		return
	}
	items := make([]principalReadView, 0)
	for _, p := range snap.Principals() {
		view := principalReadView{
			ID: p.ID, Kind: p.Kind, CreatedAt: p.CreatedAt, TokenFingerprint: p.TokenFingerprint,
			DisplayName: p.DisplayName, Namespace: p.Namespace, Role: p.Role, Status: p.Status,
			Remote: p.Remote, RemoteInstance: p.RemoteInstance, RemoteRef: p.RemoteRef, LastLoginAt: p.LastLoginAt,
		}
		items = append(items, view)
	}
	writeJSON(w, http.StatusOK, permissionList[principalReadView]{Items: items, Count: len(items)})
}

// HandleListBindings serves the effective binding per principal/agent pair.
func (h *Handler) HandleListBindings(w http.ResponseWriter, r *http.Request) {
	snap := h.managementSnapshot(w, r)
	if snap == nil {
		return
	}
	items := snap.Bindings()
	writeJSON(w, http.StatusOK, permissionList[*permissions.Binding]{Items: items, Count: len(items)})
}

// HandleGetAgentClass serves the exact class/owner/scope record the ACL reads.
func (h *Handler) HandleGetAgentClass(w http.ResponseWriter, r *http.Request) {
	snap := h.managementSnapshot(w, r)
	if snap == nil {
		return
	}
	id := mux.Vars(r)["agentid"]
	a := snap.Agent(id)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":  "AGENT_NOT_FOUND",
			"detail": fmt.Sprintf("no such agent %q: no agent class record is stored for this agent", id),
		})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// HandleListGrants excludes both revoked and expired versions AFTER keep-LAST.
func (h *Handler) HandleListGrants(w http.ResponseWriter, r *http.Request) {
	snap := h.managementSnapshot(w, r)
	if snap == nil {
		return
	}
	now := time.Now().UTC()
	items := make([]*permissions.Grant, 0)
	for _, g := range snap.Grants() {
		if g.Live(now) {
			items = append(items, g)
		}
	}
	writeJSON(w, http.StatusOK, permissionList[*permissions.Grant]{Items: items, Count: len(items)})
}
