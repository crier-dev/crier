// wait_http.go — the delivery-wait HTTP surface (CR-CHAT-034). Three routes,
// all opt-in and registered beside the six control routes only when
// CR_DAGGER_URL is set:
//
//	POST /dagger/wait              deliver to an agent and wait, bounded
//	POST /dagger/waits/{key}/resolve   record a reply on a wait (the agent)
//	GET  /dagger/waits/{key}       read a wait's state without joining it
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

// resolveWaitBody is the POST /dagger/waits/{key}/resolve body. The reply is
// the agent's output for the node, stored verbatim on the wait.
type resolveWaitBody struct {
	MessageID string          `json:"message_id,omitempty"`
	Reply     json.RawMessage `json:"reply,omitempty"`
}

// HandleDeliverAndWait serves POST /dagger/wait.
func (h *Handler) HandleDeliverAndWait(w http.ResponseWriter, r *http.Request) {
	var req WaitRequest
	if err := decodeBody(r, &req); err != nil {
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.svc.DeliverAndWait(r.Context(), req)
	if err != nil {
		writeWaitError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, res)
}

// HandleResolveWait serves POST /dagger/waits/{key}/resolve.
func (h *Handler) HandleResolveWait(w http.ResponseWriter, r *http.Request) {
	var body resolveWaitBody
	if err := decodeBody(r, &body); err != nil {
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The whole body IS the reply when no explicit member was posted: an
	// agent that answers with a bare JSON document still gets its output
	// carried on the wait.
	reply := body.Reply
	if len(reply) == 0 {
		if raw, rerr := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes)); rerr == nil && len(strings.TrimSpace(string(raw))) > 0 {
			reply = json.RawMessage(raw)
		}
	}
	res, err := h.svc.ResolveWait(mux.Vars(r)["key"], body.MessageID, reply)
	if err != nil {
		writeWaitError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, res)
}

// HandleWaitStatus serves GET /dagger/waits/{key}.
func (h *Handler) HandleWaitStatus(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.WaitStatus(mux.Vars(r)["key"])
	if err != nil {
		writeWaitError(w, err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, res)
}

// writeWaitError maps the wait surface's outcomes onto the API's status
// vocabulary. Every outcome is a NAMED body — a timeout and a cancellation
// are facts a caller must be able to distinguish from an empty success.
func writeWaitError(w http.ResponseWriter, err error) {
	var code string
	var status int
	switch {
	case errors.Is(err, ErrWaitTimeout):
		code, status = CodeDeliveryWaitTimeout, http.StatusGatewayTimeout
	case errors.Is(err, ErrWaitCancelled):
		code, status = CodeWaitCancelled, http.StatusConflict
	case errors.Is(err, ErrWaitNotFound):
		code, status = "WAIT_NOT_FOUND", http.StatusNotFound
	case errors.Is(err, ErrInvalidInput):
		httperr.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrUnconfigured):
		httperr.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":  "DAGGER_UNCONFIGURED",
			"detail": err.Error(),
		})
		return
	default:
		code, status = "DAGGER_WAIT_ERROR", http.StatusInternalServerError
	}
	httperr.WriteJSON(w, status, map[string]string{
		"error":  code,
		"detail": err.Error(),
	})
}
