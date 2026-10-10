package registry

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/gorilla/mux"
)

// This file is the management surface for the permission records
// (CR-CHAT-007 phase 1): minting a principal, creating/revoking a binding, and
// creating a grant — the WRITE half of the ACL the delivery path only reads.
//
// Arming (documented choice, per the task's "keep it simple" instruction):
// the surface exists only when a permission store is wired on the handler
// (SetPermissionsStore, which cmd/server does exactly when the delivery ACL is
// enabled), and every write additionally requires an ADMIN TOKEN — the
// CR_PERMISSIONS_ADMIN_TOKEN secret, presented as the request's Bearer. A
// deployment that has not armed the token answers every management write with
// 403 MANAGEMENT_FORBIDDEN: the records decide who may reach whom, so an
// unauthenticated principal/bind/grant mint would be a self-service hole
// through §6 (the spec's granted_by rule — "you cannot grant admin without
// admin" — has no principal-side enforcement yet, phase 1 has no login, so the
// deployment token is the granter of record).
//
// The shared auth middleware accepts EITHER the message token or the admin
// token on these paths (middleware.AuthTokens, DF-CRIER-297): a message-token
// holder is let through precisely so the named 403 below is what answers it,
// and the admin token reaches the handler here. This constant-time secret is
// therefore the SOLE authority for every write on this surface — distinct from
// CR_AUTH_TOKEN so a leaked message token cannot mint permissions, and refused
// at boot when the two are configured to the same value (config.Load).
//
// granted_by is recorded as "admin" for every record this surface writes: the
// caller is the operator, not a principal, and §6.1's field names WHO granted —
// phase 2 (login/OIDC) replaces it with the acting principal's id.

// ErrManagementNotArmed is the error text of a management write on a handler
// with no permission store, or without the admin token.
const errManagementNotArmed = "MANAGEMENT_FORBIDDEN"

type principalMintRequest struct {
	DisplayName string `json:"display_name,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Role        string `json:"role,omitempty"`
	Status      string `json:"status,omitempty"`
}

type bindingCreateRequest struct {
	Principal string `json:"principal"`
	Agent     string `json:"agent"`
	AsAgent   *bool  `json:"as_agent,omitempty"`
}

type grantCreateRequest struct {
	Principal string               `json:"principal"`
	Subject   permissions.Subject  `json:"subject"`
	Actions   []permissions.Action `json:"actions"`
	ExpiresAt *time.Time           `json:"expires_at,omitempty"`
	Note      string               `json:"note,omitempty"`
}

type agentClassRequest struct {
	Class       string   `json:"class"`
	Owner       string   `json:"owner,omitempty"`
	ReachableBy []string `json:"reachable_by,omitempty"`
}

// SetPermissionsStore wires the permission store the management surface writes
// to. Nil (the default) leaves the surface inert: every write is refused
// 403 MANAGEMENT_FORBIDDEN, and the routes answer nothing else. It is the same
// store the delivery ACL reads, so a record is in force for the very next
// delivery (§6.6 per-request evaluation).
func (h *Handler) SetPermissionsStore(s permissions.Store) { h.permissionsStore = s }

// SetPermissionsAdminToken arms the management surface's admin gate. Empty
// (the default) keeps the gate closed even with a store wired.
func (h *Handler) SetPermissionsAdminToken(token string) { h.permissionsAdminToken = token }

// managementAuthorized is the admin gate: the request must present the armed
// admin token as its Bearer, compared in constant time.
func (h *Handler) managementAuthorized(r *http.Request) bool {
	if h.permissionsStore == nil || h.permissionsAdminToken == "" {
		return false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)),
		[]byte(h.permissionsAdminToken)) == 1
}

// refuseManagement writes the named 403 every unauthorized management write
// gets — never a silent no-op and never a 404 that reads as a missing route.
func refuseManagement(w http.ResponseWriter, armed bool) {
	detail := "the permissions management surface is not armed"
	if armed {
		detail = "an admin token is required (CR_PERMISSIONS_ADMIN_TOKEN)"
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error":  errManagementNotArmed,
		"detail": detail,
	})
}

// HandleMintPrincipal mints a principal record (POST /principals). §2.1's
// shape: kind=principal, a status (default active — phase 1 has no login to
// move invited→active), an optional namespace and role.
func (h *Handler) HandleMintPrincipal(w http.ResponseWriter, r *http.Request) {
	if !h.managementAuthorized(r) {
		refuseManagement(w, h.permissionsStore != nil)
		return
	}
	var req principalMintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	p := &permissions.Principal{
		Kind:        "principal",
		DisplayName: req.DisplayName,
		Namespace:   req.Namespace,
		Status:      permissions.PrincipalStatus(req.Status),
	}
	if p.Status == "" {
		p.Status = permissions.PrincipalActive
	}
	if req.Role != "" {
		p.Role = permissions.Role(req.Role)
	}
	p.ID = permissions.NewRecordID("prin_")
	if err := h.permissionsStore.Append(r.Context(), p.Record(time.Now().UTC())); err != nil {
		slog.Error("mint principal", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failure"})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// HandleCreateBinding records a binding (POST /bindings): §2.3's speech right.
// as_agent defaults to true — the only value this version defines.
func (h *Handler) HandleCreateBinding(w http.ResponseWriter, r *http.Request) {
	if !h.managementAuthorized(r) {
		refuseManagement(w, h.permissionsStore != nil)
		return
	}
	var req bindingCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.Principal) == "" || strings.TrimSpace(req.Agent) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "principal and agent are required"})
		return
	}
	asAgent := true
	if req.AsAgent != nil {
		asAgent = *req.AsAgent
	}
	b := &permissions.Binding{
		Kind:      "binding",
		Principal: req.Principal,
		Agent:     req.Agent,
		AsAgent:   asAgent,
		CreatedBy: "admin",
	}
	b.ID = permissions.NewRecordID("bind_")
	if err := h.permissionsStore.Append(r.Context(), b.Record(time.Now().UTC())); err != nil {
		slog.Error("create binding", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failure"})
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// HandleCreateGrant records a grant (POST /grants): §6.1's ACL entry. The
// action set and subject are validated by the record itself, so a malformed
// action or subject is a 400 naming the accepted vocabulary, never a stored
// line the ACL would have to guess about.
func (h *Handler) HandleCreateGrant(w http.ResponseWriter, r *http.Request) {
	if !h.managementAuthorized(r) {
		refuseManagement(w, h.permissionsStore != nil)
		return
	}
	var req grantCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	g := &permissions.Grant{
		Kind:      "grant",
		Principal: req.Principal,
		Subject:   req.Subject,
		Actions:   req.Actions,
		GrantedBy: "admin",
		GrantedAt: time.Now().UTC(),
		ExpiresAt: req.ExpiresAt,
		Note:      req.Note,
	}
	g.ID = permissions.NewRecordID("grant_")
	rec := g.Record(time.Now().UTC())
	if err := rec.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.permissionsStore.Append(r.Context(), rec); err != nil {
		slog.Error("create grant", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failure"})
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// HandleRevokeGrant tombstones a grant (POST /grants/{id}/revoke): §6.6 — the
// record stays, the tombstone is appended, and the very next delivery sees it.
func (h *Handler) HandleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	if !h.managementAuthorized(r) {
		refuseManagement(w, h.permissionsStore != nil)
		return
	}
	id := mux.Vars(r)["grantid"]
	if strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grant id is required"})
		return
	}
	if err := permissions.RevokeGrant(context.Background(), h.permissionsStore, id, "admin", time.Now().UTC()); err != nil {
		if errors.Is(err, permissions.ErrGrantNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		slog.Error("revoke grant", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failure"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"revoked": id})
}

// HandleSetAgentClass records an agent-class row (POST /agentclass): §3.1's
// class/owner data, kept as permission.agent records (§8.1 item 3 — the
// registry row itself does not carry class yet). This is the switch that ARMS
// the ACL for one agent: an agent with no class record keeps the shipped
// legacy posture (§6.4 rule 2 / §8.1), while a classed `personal` row is
// owner-only and a classed `service` row is grant/scope-reach/admin only
// (§3.2, checker.go evaluateAgent). Validating the record before Append means
// a class without a coherent owner answer is a 400, never a stored line the
// checker would have to guess about.
func (h *Handler) HandleSetAgentClass(w http.ResponseWriter, r *http.Request) {
	if !h.managementAuthorized(r) {
		refuseManagement(w, h.permissionsStore != nil)
		return
	}
	var req agentClassRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if !permissions.ValidClass(permissions.AgentClass(req.Class)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "INVALID_CLASS",
			"detail": "class must be \"personal\" or \"service\" (§3.1); empty keeps the unclassed legacy posture, which is what already holds when this record is absent",
		})
		return
	}
	a := &permissions.AgentInfo{
		ID:    mux.Vars(r)["agentid"],
		Class: permissions.AgentClass(req.Class),
		Owner: req.Owner,
	}
	if len(req.ReachableBy) > 0 {
		a.Scopes = append(a.Scopes, permissions.Scope{Reach: permissions.Reach{Principals: req.ReachableBy}})
	}
	if err := a.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.permissionsStore.Append(r.Context(), a.Record(time.Now().UTC())); err != nil {
		slog.Error("set agent class", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store failure"})
		return
	}
	writeJSON(w, http.StatusCreated, a)
}
