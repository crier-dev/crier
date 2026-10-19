package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
)

// deliveryFailure projects transport evidence without treating a wrapper HTTP
// status as the target's answer or embedding an HTTP JSON body in human detail.
// It never grants a new delivery attempt; same-key replay policy stays separate.
func deliveryFailure(target string, err error) DeliveryOutcome {
	o := DeliveryOutcome{Target: target, Outcome: OutcomeRefused, Code: "DELIVERY_REFUSED", Reason: err.Error()}
	var dispatch *registry.DispatchError
	if errors.As(err, &dispatch) {
		o.Reason = http.StatusText(dispatch.StatusCode)
		o.Code = dispatch.Code
		if o.Code == "" {
			o.Code = "DELIVERY_REFUSED"
		}
		o.Retryable = dispatch.StatusCode == http.StatusTooManyRequests || dispatch.StatusCode >= 500
		// The registry owns several HTTP body types (including guard metadata).
		// Decode only their string diagnostic fields; never stringify the body
		// into a transcript reason, and never classify a busy lock by status alone.
		body, marshalErr := json.Marshal(dispatch.Response)
		var fields struct {
			Code   string `json:"code"`
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if marshalErr == nil && json.Unmarshal(body, &fields) == nil {
			o.Reason = fields.Error
			if fields.Detail != "" {
				if o.Reason != "" {
					o.Reason += ": "
				}
				o.Reason += fields.Detail
			}
			if fields.Code != "" {
				o.Code = fields.Code
			} else if fields.Error != "" && strings.Trim(fields.Error, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == "" {
				// Existing HTTP error fields also carry symbolic codes.
				o.Code = fields.Error
			}
		}
		if o.Reason == "" {
			o.Reason = http.StatusText(dispatch.StatusCode)
		}
		if dispatch.Code == "DELIVERY_IN_PROGRESS" {
			o.Outcome, o.Retryable = OutcomeInProgress, true
			return o
		}
	}
	var blocking *webhook.BlockingDeliveryError
	if errors.As(err, &blocking) {
		o.Reason = blocking.Error()
		o.Retryable = !errors.Is(err, webhook.ErrPermanent)
		if blocking.Cut {
			o.Outcome, o.Code = OutcomeBudgetCut, "DELIVERY_BUDGET_CUT"
		} else if blocking.TargetStatus > 0 {
			o.Code = fmt.Sprintf("TARGET_HTTP_%d", blocking.TargetStatus)
			o.Reason = fmt.Sprintf("target answered status %d; %s", blocking.TargetStatus, blocking.Error())
		}
	}
	if errors.Is(err, registry.ErrAgentNotFound) {
		o.Code = "AGENT_NOT_FOUND"
		o.Reason = fmt.Sprintf("no such agent %q (AGENT_NOT_FOUND): %v", target, err)
	}
	return o
}
