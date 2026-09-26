// a2apush.go — the A2A push-notification configuration operations
// (INT-A2A-005, specs/A2A-OPTION.md §5.5).
//
// Four JSON-RPC methods (§9.4.7) served by the SAME binding as SendMessage: the
// create/get/list/delete operations over a TaskPushNotificationConfig. There is
// no HTTP+JSON/REST route here (/tasks/{id}/pushNotificationConfigs…) because
// crier implements exactly ONE A2A binding — JSON-RPC 2.0 over HTTP, with SSE
// for streaming (§2 of the option spec); §11's REST binding is declared MAY and
// is not built, and a third A2A route would be a surface the option does not
// have.
//
// The configuration is a VIEW over crier's existing per-agent webhook config
// (§3's mapping table), and it is reached through crier's own write path rather
// than a new one: a create/delete is the body POST/PATCH /agents/{id} already
// accepts, handed to that route's OWN handler (registry.Handler.HandleUpdateAgent,
// the function PATCH /agents/{id} is registered with) — exactly the reuse
// deliverTo performs for SendMessage. Nothing about delivery is re-implemented:
// the notification is POSTed by the shipped webhook driver, with the config's own
// retries/timeout/batch/delivery_mode, and the payload shape §4.3.3 requires is
// the config's schema, expressed as a crier template.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
)

// pushCreate serves CreateTaskPushNotificationConfig (§3.1.7).
func (h *a2aHandler) pushCreate(w http.ResponseWriter, r *http.Request, req *a2a.RPCRequest) {
	params, rpcErr := a2a.DecodeCreatePushParams(req.Params)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	target, rpcErr := h.target(params.Tenant)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}

	row := pushRow(target)
	write, rpcErr := a2a.ValidatePushCreate(params, row)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}

	cfg := pushWebhookConfig(target.Webhook, write)
	body, err := json.Marshal(map[string]any{"webhook": cfg})
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"the push configuration could not be encoded: "+err.Error())))
		return
	}

	status, respBody, err := h.patchTo(target.ID, body, r.Context())
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"crier's registry update path could not be reached: "+err.Error())))
		return
	}
	if status < 200 || status > 299 {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.PushWriteRefused(status, respBody)))
		return
	}

	// The write's own answer is the updated row, so the projection reads what
	// crier actually stored rather than what this handler intended to store.
	var updated registry.Agent
	if err := json.Unmarshal(respBody, &updated); err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"crier's registry answer could not be read: "+err.Error())))
		return
	}
	rowOut := pushRow(&updated)
	if !rowOut.Configured {
		// crier accepted the write and answered without a push channel. That
		// is a server-side inconsistency, and it is reported as one: answering
		// -32003 (the capability error) would be a false statement about an
		// agent whose configuration crier has just accepted.
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"crier accepted the push configuration but answered without one; the agent's webhook config could not be read back")))
		return
	}
	cfgOut, rpcErr := a2a.ProjectPushConfig(target.ID, params.TaskID, rowOut)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	h.writeRPC(w, a2a.SuccessResponse(req.ID, cfgOut))
}

// pushGet serves GetTaskPushNotificationConfig (§3.1.8).
func (h *a2aHandler) pushGet(w http.ResponseWriter, r *http.Request, req *a2a.RPCRequest) {
	params, target, row, ok := h.pushRef(w, req)
	if !ok {
		return
	}
	if rpcErr := a2a.MatchPushConfigID(target.ID, params.ID, row); rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	cfg, rpcErr := a2a.ProjectPushConfig(target.ID, params.TaskID, row)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	h.writeRPC(w, a2a.SuccessResponse(req.ID, cfg))
}

// pushList serves ListTaskPushNotificationConfigs (§3.1.9).
//
// The answer holds at most one configuration, because crier has one push channel
// per agent. That is stated in the response rather than implied: no page token
// is issued, and one the client sends is refused by the params decoder.
func (h *a2aHandler) pushList(w http.ResponseWriter, r *http.Request, req *a2a.RPCRequest) {
	params, rpcErr := a2a.DecodeListPushConfigsParams(req.Params)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	target, rpcErr := h.target(params.Tenant)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	cfg, rpcErr := a2a.ProjectPushConfig(target.ID, params.TaskID, pushRow(target))
	if rpcErr != nil {
		// The §3.3.4 capability answer: an agent with no push channel has no
		// configuration to list, and says so instead of listing nothing.
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	h.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.ListPushConfigsResponse{
		Configs: []a2a.TaskPushNotificationConfig{cfg},
	}))
}

// pushDelete serves DeleteTaskPushNotificationConfig (§3.1.10).
//
// Deleting the push configuration is deleting the agent's webhook config —
// there is one channel, so there is one thing to remove, and §3.1.10's "no
// further notifications will be sent to the configured webhook after deletion"
// is exactly crier's own webhook removal (PATCH /agents/{id} with
// {"webhook":null}), reached through that route's handler.
//
// A SECOND delete answers PushNotificationNotSupportedError rather than
// succeeding silently: §3.1.10 lists that error for this operation, and after
// the first delete the capability is genuinely false — which is precisely what
// §3.3.4 requires this surface to say.
func (h *a2aHandler) pushDelete(w http.ResponseWriter, r *http.Request, req *a2a.RPCRequest) {
	params, target, row, ok := h.pushRef(w, req)
	if !ok {
		return
	}
	if rpcErr := a2a.MatchPushConfigID(target.ID, params.ID, row); rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}
	body, err := json.Marshal(map[string]any{"webhook": nil})
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"the removal could not be encoded: "+err.Error())))
		return
	}
	status, respBody, err := h.patchTo(target.ID, body, r.Context())
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"crier's registry update path could not be reached: "+err.Error())))
		return
	}
	if status < 200 || status > 299 {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.PushWriteRefused(status, respBody)))
		return
	}
	h.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.DeletePushConfigResult{
		Deleted: true,
		ID:      params.ID,
		Tenant:  target.ID,
	}))
}

// pushRef is the shared preamble of Get and Delete: decode the addressing
// params, resolve the agent through the per-agent half of the A2A gate, and
// answer the §3.3.4 capability error when the agent has no push channel.
func (h *a2aHandler) pushRef(w http.ResponseWriter, req *a2a.RPCRequest) (*a2a.PushConfigRefParams, *registry.Agent, a2a.PushRow, bool) {
	params, rpcErr := a2a.DecodePushConfigRefParams(req.Params)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return nil, nil, a2a.PushRow{}, false
	}
	target, rpcErr := h.target(params.Tenant)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return nil, nil, a2a.PushRow{}, false
	}
	row := pushRow(target)
	if !row.Configured {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.PushNotSupportedError(target.ID)))
		return nil, nil, a2a.PushRow{}, false
	}
	return params, target, row, true
}

// pushRow projects the registry row onto the plain evidence the a2a package
// reads — the same one-way projection the Agent Card performs, so the card's
// capabilities.pushNotifications and the capability answer here can never
// disagree (both ask "does this row carry a webhook?").
func pushRow(agent *registry.Agent) a2a.PushRow {
	row := a2a.PushRow{Tenant: agent.ID}
	if agent.Webhook == nil {
		return row
	}
	row.Configured = true
	row.URL = agent.Webhook.URL
	row.AuthType = string(agent.Webhook.AuthType)
	row.AuthRefSet = agent.Webhook.AuthValueRef != ""
	row.CustomSchemaSet = agent.Webhook.CustomSchema != nil
	return row
}

// pushWebhookConfig applies a validated A2A write to the agent's existing
// webhook config.
//
// It CHANGES only the fields the A2A view owns and preserves everything else —
// retries, timeout_ms, batch, delivery_mode, schema_template and, unless the
// request said otherwise, the named secret. Those are crier's own knobs, the
// A2A configuration object has no field for them, and resetting them would be
// the silent loss DF-CRIER-279 is about: "delivery uses the existing
// retry/batch machinery" is only true if the machinery's configuration survives
// the operation.
//
// The notification payload shape (§4.3.3's StreamResponse) is installed as the
// config's custom_schema — crier's own bring-your-own-schema field, which its
// resolution rule already prefers over a named template. Nothing about the
// delivery driver changes: it renders this schema exactly as it renders any
// other.
func pushWebhookConfig(existing *webhook.Config, write *a2a.PushWrite) *webhook.Config {
	cfg := webhook.Config{}
	if existing != nil {
		cfg = *existing
	}
	cfg.URL = write.URL

	shape := a2a.PushNotificationShape()
	headers := make(map[string]string, len(shape.Headers))
	for k, v := range shape.Headers {
		headers[k] = v
	}
	cfg.CustomSchema = &webhook.CustomSchema{
		RequestShape: &webhook.RequestShape{
			Method:  http.MethodPost,
			Headers: headers,
			Body:    shape.Body,
		},
		ResponseMap: shape.ResponseMap,
	}

	switch write.AuthType {
	case a2a.AuthBearer:
		cfg.AuthType = webhook.AuthBearer
	case a2a.AuthNone:
		cfg.AuthType = webhook.AuthNone
	case "":
		// The request said nothing about authentication: the row's own posture
		// stands.
	}
	if write.ClearAuthRef {
		cfg.AuthValueRef = ""
	}
	return &cfg
}

// patchTo runs a registry update through crier's own PATCH /agents/{id} handler
// — the very function that route is registered with — and returns the status and
// body it wrote, mirroring deliverTo.
//
// Reusing that handler is what makes the write honest: the strict webhook member
// decode, webhook.Config.Validate, the store update path and the agent-owned
// signature gate (requireAgent, enforced when CR_REQUIRE_AGENT_SIG is on) are
// crier's own, reached unchanged. An A2A client therefore cannot write something
// crier's own route would refuse, and it cannot bypass a gate crier's own route
// applies — a refusal comes back with crier's status and body verbatim
// (PushWriteRefused).
func (h *a2aHandler) patchTo(agentID string, body []byte, parent context.Context) (int, []byte, error) {
	if h.patch == nil {
		return 0, nil, errors.New("the registry update handler is not wired")
	}
	inner, err := http.NewRequestWithContext(parent, http.MethodPatch,
		"/agents/"+agentID, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build the registry update request: %w", err)
	}
	inner.Header.Set("Content-Type", "application/json")
	// The route reads the target from the path vars; setting them directly is
	// what the router would have done had this arrived on the wire.
	inner = mux.SetURLVars(inner, map[string]string{"id": agentID})

	rec := &captureWriter{header: http.Header{}}
	h.patch(rec, inner)
	slog.Debug("A2A push configuration write",
		"agent_id", agentID, "status", rec.statusOrDefault())
	return rec.statusOrDefault(), rec.body.Bytes(), nil
}
