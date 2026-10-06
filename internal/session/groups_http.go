// groups_http.go — the group API surface (CR-CHAT-022, D8).
//
// A NAMED group is a curated, editable roster addressed as `@team:x`; a
// capability target is the DYNAMIC pool `POST /capabilities/{capability}/inbox`
// already serves (round-robin over live holders). Both are addressable —
// `@team:x` and `@cap:y` — and neither may silently become the other
// (specs/CHAT-ADDRESSING.md §1.4): a named group's audience is enumerable and
// recordable BEFORE the send; a capability's holder is decided at delivery
// time and recorded then.
//
// Endpoints, on the handler's own conventions (apiError bodies, realm scoping
// via X-Crier-Namespace, unknown fields refused):
//
//	POST   /groups                     create a group
//	GET    /groups                     list every group (the realm's)
//	GET    /groups/{name}              read one roster — INSPECTABLE: who is
//	                                   in it, who created it, who last edited it
//	PATCH  /groups/{name}/members      add/remove members (one edit event)
//
// The send side needs no new route: POST /sessions/{id}/messages already
// accepts a `group` audience target (D8, §3.4 rule 7), and the roster is
// resolved HERE, fresh, at send time — a roster edit routes the NEXT send to
// the CURRENT members, never a cached set (§1.4 consequence 1).
package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
)

// groupView is the inspectable roster (acceptance d): who is in it, who
// created it, who last edited it.
type groupView struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace,omitempty"`
	Members   []string  `json:"members"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// groupsResponse is the GET /groups body.
type groupsResponse struct {
	Groups []groupView `json:"groups"`
	Count  int         `json:"count"`
}

type createGroupRequest struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Members   []string `json:"members,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
}

// updateGroupMembersRequest is the PATCH /groups/{name}/members body: the
// members to add and the members to remove, applied in that order — an id
// named in both lists is a refusal, not a silent ordering.
type updateGroupMembersRequest struct {
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	Actor  string   `json:"actor,omitempty"`
}

// groups returns the wired GroupStore, or nil when the surface is
// unconfigured (a send with a group target is then a recorded skip).
func (h *Handler) groups() GroupStore { return h.opts.Groups }

// HandleListGroups serves GET /groups — every group in the request's realm,
// sorted by name.
func (h *Handler) HandleListGroups(w http.ResponseWriter, r *http.Request) {
	store := h.groups()
	if store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "GROUPS_UNCONFIGURED",
			"no group store is wired, so there are no named groups to list")
		return
	}
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return
	}
	all, err := store.List(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
		return
	}
	out := groupsResponse{Groups: make([]groupView, 0, len(all))}
	for _, g := range all {
		if namespaceOf(g) != realm {
			continue
		}
		out.Groups = append(out.Groups, groupViewOf(g))
	}
	out.Count = len(out.Groups)
	writeJSON(w, http.StatusOK, out)
}

// HandleGetGroup serves GET /groups/{name} — the CURRENT roster.
func (h *Handler) HandleGetGroup(w http.ResponseWriter, r *http.Request) {
	store := h.groups()
	if store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "GROUPS_UNCONFIGURED",
			"no group store is wired, so there are no named groups to read")
		return
	}
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return
	}
	name := muxVar(r, "name")
	g, err := store.Get(r.Context(), name)
	if err != nil {
		writeGroupStoreError(w, err, name)
		return
	}
	if namespaceOf(g) != realm {
		// The realm wall: a group of another realm reads as absent — the
		// same rule a session of another realm follows (loadScoped).
		writeAPIError(w, http.StatusNotFound, "GROUP_NOT_FOUND",
			fmt.Sprintf("group %q does not exist in this realm", name))
		return
	}
	writeJSON(w, http.StatusOK, groupViewOf(g))
}

// HandleCreateGroup serves POST /groups.
func (h *Handler) HandleCreateGroup(w http.ResponseWriter, r *http.Request) {
	store := h.groups()
	if store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "GROUPS_UNCONFIGURED",
			"no group store is wired, so a group cannot be created")
		return
	}
	var req createGroupRequest
	if !decodeBody(w, r, &req) {
		return
	}
	realm, err := h.realm(r, req.Namespace)
	if err != nil {
		writeRealmError(w, err)
		return
	}
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = strings.TrimSpace(r.Header.Get(registryHeaderAgentID))
	}
	if createdBy == "" {
		writeAPIError(w, http.StatusBadRequest, "CREATED_BY_REQUIRED",
			"created_by (or the X-Agent-ID header) is required: a roster records who opened it (§1.4)")
		return
	}
	now := h.now()
	g := &Group{
		Name:      strings.TrimSpace(req.Name),
		Namespace: realm,
		Members:   req.Members,
		CreatedAt: now,
		CreatedBy: createdBy,
		UpdatedAt: now,
		UpdatedBy: createdBy,
	}
	if err := store.Create(r.Context(), g); err != nil {
		writeGroupStoreError(w, err, g.Name)
		return
	}
	writeJSON(w, http.StatusCreated, groupViewOf(g))
}

// HandleUpdateGroupMembers serves PATCH /groups/{name}/members — one edit
// event over the CURRENT roster. The edit is read-modify-write against the
// store, so the NEXT send resolves the new set (§1.4 consequence 1); sends
// already in flight resolved their own audience before it.
func (h *Handler) HandleUpdateGroupMembers(w http.ResponseWriter, r *http.Request) {
	store := h.groups()
	if store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "GROUPS_UNCONFIGURED",
			"no group store is wired, so a roster cannot be edited")
		return
	}
	realm, err := h.realm(r, "")
	if err != nil {
		writeRealmError(w, err)
		return
	}
	name := muxVar(r, "name")
	var req updateGroupMembersRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Add) == 0 && len(req.Remove) == 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"the edit names nothing: give members to add, remove, or both")
		return
	}
	for _, id := range req.Add {
		if !ValidGroupName(strings.TrimSpace(id)) {
			writeAPIError(w, http.StatusBadRequest, "INVALID_MEMBER",
				fmt.Sprintf("member id %q is not an ident", id))
			return
		}
	}
	cur, err := store.Get(r.Context(), name)
	if err != nil {
		writeGroupStoreError(w, err, name)
		return
	}
	if namespaceOf(cur) != realm {
		writeAPIError(w, http.StatusNotFound, "GROUP_NOT_FOUND",
			fmt.Sprintf("group %q does not exist in this realm", name))
		return
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = strings.TrimSpace(r.Header.Get(registryHeaderAgentID))
	}
	next := &Group{
		Name:      cur.Name,
		Namespace: cur.Namespace,
		CreatedAt: cur.CreatedAt,
		CreatedBy: cur.CreatedBy,
		UpdatedAt: h.now(),
		UpdatedBy: actor,
	}
	seen := map[string]bool{}
	for _, m := range cur.Members {
		seen[m] = true
	}
	remove := map[string]bool{}
	for _, id := range req.Remove {
		remove[strings.TrimSpace(id)] = true
	}
	members := make([]string, 0, len(cur.Members)+len(req.Add))
	for _, m := range cur.Members {
		if remove[m] {
			continue
		}
		members = append(members, m)
	}
	for _, id := range req.Add {
		m := strings.TrimSpace(id)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		members = append(members, m)
	}
	next.Members = members
	if err := store.Update(r.Context(), next); err != nil {
		writeGroupStoreError(w, err, name)
		return
	}
	writeJSON(w, http.StatusOK, groupViewOf(next))
}

// registryHeaderAgentID is the shipped identity header a caller names itself
// with on a body-less-actor request (the same header authorizeRead reads).
const registryHeaderAgentID = "X-Agent-ID"

// namespaceOf reads a group's realm, tolerating a nil pointer.
func namespaceOf(g *Group) string {
	if g == nil {
		return ""
	}
	return namespace.Canonical(g.Namespace)
}

func groupViewOf(g *Group) groupView {
	v := groupView{
		Name:      g.Name,
		Namespace: namespace.Canonical(g.Namespace),
		CreatedAt: g.CreatedAt,
		CreatedBy: g.CreatedBy,
		UpdatedAt: g.UpdatedAt,
		UpdatedBy: g.UpdatedBy,
	}
	if len(g.Members) > 0 {
		v.Members = g.Members
	} else {
		v.Members = []string{}
	}
	return v
}

// resolveGroupRoster reads the CURRENT rosters of a message's group targets,
// fresh, at send time (§1.4 consequence 1). Unknown names are simply absent
// from the returned map — FanoutRecipients reports them as skips — and a nil
// store yields a nil roster, so a group target is a recorded skip exactly as
// before this surface existed.
func (h *Handler) resolveGroupRoster(ctx context.Context, aud Audience) map[string][]string {
	var names []string
	for _, t := range aud.Targets {
		if t.Kind == TargetGroup {
			names = append(names, t.ID)
		}
	}
	if len(names) == 0 || h.groups() == nil {
		return nil
	}
	roster := make(map[string][]string, len(names))
	for _, name := range names {
		g, err := h.groups().Get(ctx, name)
		if err != nil || g == nil {
			continue
		}
		roster[name] = g.Members
	}
	return roster
}

// writeGroupStoreError maps a GroupStore failure onto the surface's error
// vocabulary.
func writeGroupStoreError(w http.ResponseWriter, err error, name string) {
	switch {
	case errors.Is(err, ErrGroupNotFound):
		writeAPIError(w, http.StatusNotFound, "GROUP_NOT_FOUND",
			fmt.Sprintf("group %q does not exist", name))
	case errors.Is(err, ErrGroupExists):
		writeAPIError(w, http.StatusConflict, "GROUP_EXISTS",
			fmt.Sprintf("group %q already exists", name))
	case errors.Is(err, ErrInvalidGroup):
		writeAPIError(w, http.StatusBadRequest, "INVALID_GROUP", err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
	}
}
