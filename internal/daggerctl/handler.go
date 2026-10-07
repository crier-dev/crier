package daggerctl

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/httperr"
)

// maxBodyBytes bounds a control request body. A control call names a prompt, a
// skill or a node — never a payload — so the cap is small on purpose.
const maxBodyBytes = 1 << 20

// Handler serves the dagger control routes. It is a thin translation layer:
// decode the request, call the Service, render the record. Every failure is a
// named JSON error through internal/httperr, the same envelope the rest of the
// API uses.
type Handler struct {
	svc *Service
}

// NewHandler wraps a control Service.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// rewindBody is the POST /dagger/runs/{id}/rewind request.
type rewindBody struct {
	NodeID string `json:"node_id"`
}

// runSkillBody is the POST /dagger/skills/{skill}/run request.
type runSkillBody struct {
	AgentID string         `json:"agent_id"`
	Args    map[string]any `json:"args,omitempty"`
	Target  string         `json:"target,omitempty"`
}

// HandleCreateRun is POST /dagger/runs — start a prompt-driven DAG. The body is
// exactly daggerctl.CreateRunRequest's shape ({agent_id, prompt}), decoded
// strictly, so there is no second body type to keep in step with it.
func (h *Handler) HandleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req CreateRunRequest
	if err := decodeBody(r, &req); err != nil {
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := h.svc.CreateRun(r.Context(), req)
	if err != nil {
		writeControlError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusCreated, rec)
}

// HandleGetRun is GET /dagger/runs/{id} — read a run's record back.
func (h *Handler) HandleGetRun(w http.ResponseWriter, r *http.Request) {
	rec, err := h.svc.RunStatus(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeControlError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, rec)
}

// HandleCancel is POST /dagger/runs/{id}/cancel.
func (h *Handler) HandleCancel(w http.ResponseWriter, r *http.Request) {
	rec, err := h.svc.Cancel(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeControlError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, rec)
}

// HandleResume is POST /dagger/runs/{id}/resume.
func (h *Handler) HandleResume(w http.ResponseWriter, r *http.Request) {
	rec, err := h.svc.Resume(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		writeControlError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, rec)
}

// HandleRewind is POST /dagger/runs/{id}/rewind.
func (h *Handler) HandleRewind(w http.ResponseWriter, r *http.Request) {
	var body rewindBody
	if err := decodeBody(r, &body); err != nil {
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := h.svc.Rewind(r.Context(), mux.Vars(r)["id"], body.NodeID)
	if err != nil {
		writeControlError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, rec)
}

// HandleRunSkill is POST /dagger/skills/{skill}/run.
func (h *Handler) HandleRunSkill(w http.ResponseWriter, r *http.Request) {
	var body runSkillBody
	if err := decodeBody(r, &body); err != nil {
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := h.svc.RunSkill(r.Context(), RunSkillRequest{
		AgentID: body.AgentID,
		Skill:   mux.Vars(r)["skill"],
		Args:    body.Args,
		Target:  body.Target,
	})
	if err != nil {
		writeControlError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusCreated, rec)
}

// decodeBody strictly decodes a control request body. A member the shape does
// not declare is REJECTED, not silently dropped — the same rule the inbox
// delivery path applies (DF-CRIER-180), applied here on a two-field body.
// An empty body is an empty object, so a body-less cancel/resume still works.
func decodeBody(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return errors.New("could not read request body")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid request body: " + err.Error())
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid request body: unexpected trailing data")
	}
	return nil
}

// writeControlError maps a Service error onto the API's status vocabulary. The
// unconfigured case is a 503 with its own body naming what to set, because "the
// surface exists but has no executor" is an operator action, not a bad request.
func writeControlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrRunNotFound):
		httperr.WriteJSONError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrUnconfigured):
		httperr.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":  "DAGGER_UNCONFIGURED",
			"detail": err.Error(),
		})
	case errors.Is(err, ErrBridge):
		httperr.WriteJSONError(w, http.StatusBadGateway, err.Error())
	case errors.Is(err, ErrUnknownTarget):
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
	default:
		httperr.WriteJSONError(w, http.StatusInternalServerError, "dagger run store unavailable")
	}
}
