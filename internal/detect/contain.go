package detect

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// Containment action names, in the order they are performed. The order is
// load-bearing: outbound is paused first (nothing left can leave the
// compromised agent), then its in-flight claims are released (so its unacked
// messages go back to the queue instead of dying with it), then it is
// quarantined (the delivery path refuses both directions from here on), and
// only then is the registry row removed.
const (
	// ActionPauseWebhooks stops the agent's outbound webhook lane and drops
	// what it had queued.
	ActionPauseWebhooks = "pause_webhooks"
	// ActionRevokeLeases releases the unacked messages the agent had claimed.
	ActionRevokeLeases = "revoke_leases"
	// ActionQuarantine makes the delivery path refuse sends from and
	// deliveries to this agent.
	ActionQuarantine = "quarantine"
	// ActionUnregister removes the registry row.
	ActionUnregister = "unregister"
)

// ActionResult statuses. "unsupported" is an honest third state: a store or a
// driver that cannot perform the action says so, instead of reporting a
// success it did not achieve.
const (
	ActionStatusOK          = "ok"
	ActionStatusUnsupported = "unsupported"
	ActionStatusError       = "error"
)

// ActionResult is one of the containment steps, with what it really did.
type ActionResult struct {
	Action string `json:"action"`
	Status string `json:"status"`
	// Count is the number of things the action affected: queued deliveries
	// dropped (pause_webhooks) or leases released (revoke_leases).
	Count  int    `json:"count,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Containment is the outcome of one kill-switch call — the whole answer to
// "what did that one call actually do?".
type Containment struct {
	AgentID string         `json:"agent_id"`
	At      string         `json:"at"`
	Reason  string         `json:"reason,omitempty"`
	Actions []ActionResult `json:"actions"`
	// Contained is true only when the agent can no longer send, receive, or
	// hold a claim: quarantine, unregister and the outbound pause all
	// succeeded.
	Contained bool `json:"contained"`
	// Warnings names every action that did not fully succeed, so a partial
	// containment can never read as a clean one.
	Warnings []string `json:"warnings,omitempty"`
}

// WasQuarantined reports whether the agent was already contained before this
// call (an operator re-running the switch learns it was idempotent, not that
// something new happened).
func (c Containment) WasQuarantined() bool {
	for _, a := range c.Actions {
		if a.Action == ActionQuarantine {
			return a.Status == ActionStatusOK && strings.Contains(a.Detail, "already")
		}
	}
	return false
}

// Contain performs the single-call kill-switch on agentID: unregister +
// revoke leases + pause webhooks + quarantine, each reported individually.
//
// It never returns an error: a containment that partly failed is still a
// containment to report, and the caller must see which step failed rather than
// an opaque failure that hides the steps that DID work.
func (d *Detector) Contain(agentID, reason string) Containment {
	at := d.now().UTC()
	out := Containment{AgentID: agentID, At: at.Format(time.RFC3339Nano), Reason: reason}

	// 1. Pause the outbound webhook lane.
	if d.webhooks == nil {
		out.Actions = append(out.Actions, ActionResult{
			Action: ActionPauseWebhooks, Status: ActionStatusUnsupported,
			Detail: "no webhook driver is wired into this server",
		})
	} else {
		n, err := d.webhooks.PauseAgent(agentID)
		switch {
		case err != nil:
			out.Actions = append(out.Actions, ActionResult{
				Action: ActionPauseWebhooks, Status: ActionStatusError, Detail: err.Error(),
			})
		default:
			out.Actions = append(out.Actions, ActionResult{
				Action: ActionPauseWebhooks, Status: ActionStatusOK, Count: n,
				Detail: fmt.Sprintf("outbound webhook lane paused, %d queued deliveries dropped", n),
			})
		}
	}

	// 2. Release the agent's unacked leases.
	revoker, ok := d.store.(LeaseRevoker)
	if d.store == nil || !ok {
		out.Actions = append(out.Actions, ActionResult{
			Action: ActionRevokeLeases, Status: ActionStatusUnsupported,
			Detail: "the wired store cannot revoke leases (a remote store proxies reads and writes to another server)",
		})
	} else {
		n, err := revoker.RevokeLeases(agentID)
		switch {
		case err != nil:
			out.Actions = append(out.Actions, ActionResult{
				Action: ActionRevokeLeases, Status: ActionStatusError, Detail: err.Error(),
			})
		default:
			out.Actions = append(out.Actions, ActionResult{
				Action: ActionRevokeLeases, Status: ActionStatusOK, Count: n,
				Detail: fmt.Sprintf("%d unacked lease(s) released back to the queue", n),
			})
		}
	}

	// 3. Quarantine: from here the delivery path refuses both directions.
	already := d.Quarantined(agentID)
	d.mu.Lock()
	d.quarantined[agentID] = Quarantine{
		AgentID: agentID,
		At:      out.At,
		Reason:  reason,
	}
	d.mu.Unlock()
	detail := "delivery path now refuses sends from and deliveries to this agent"
	if already {
		detail = "already quarantined; " + detail
	}
	out.Actions = append(out.Actions, ActionResult{Action: ActionQuarantine, Status: ActionStatusOK, Detail: detail})

	// 4. Unregister the registry row.
	if d.store == nil {
		out.Actions = append(out.Actions, ActionResult{
			Action: ActionUnregister, Status: ActionStatusUnsupported,
			Detail: "no registry store is wired into this server",
		})
	} else if err := d.store.Unregister(agentID); err != nil {
		if errors.Is(err, registry.ErrAgentNotFound) {
			out.Actions = append(out.Actions, ActionResult{
				Action: ActionUnregister, Status: ActionStatusOK,
				Detail: "no registry row to remove (the agent was not registered)",
			})
		} else {
			out.Actions = append(out.Actions, ActionResult{
				Action: ActionUnregister, Status: ActionStatusError, Detail: err.Error(),
			})
		}
	} else {
		out.Actions = append(out.Actions, ActionResult{
			Action: ActionUnregister, Status: ActionStatusOK, Detail: "registry row removed",
		})
	}

	// Contained means every action that could run, ran: the two that make the
	// agent harmless (quarantine, unregister) and the outbound pause. An
	// unsupported lease revoker is a warning, not a containment failure — but
	// it is never silent.
	out.Contained = true
	for _, a := range out.Actions {
		switch {
		case a.Status == ActionStatusError:
			out.Contained = false
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %s", a.Action, a.Detail))
		case a.Status == ActionStatusUnsupported:
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %s", a.Action, a.Detail))
		}
	}
	if d.store == nil {
		out.Contained = false
	}

	d.record(Entry{
		At:      out.At,
		Kind:    KindContainment,
		Target:  agentID,
		Verdict: containmentVerdict(out),
		Detail:  containmentSummary(out),
	})
	return out
}

func containmentVerdict(c Containment) string {
	if c.Contained {
		return "contained"
	}
	return "partial"
}

func containmentSummary(c Containment) string {
	parts := make([]string, 0, len(c.Actions))
	for _, a := range c.Actions {
		parts = append(parts, fmt.Sprintf("%s=%s(%d)", a.Action, a.Status, a.Count))
	}
	s := strings.Join(parts, " ")
	if c.Reason != "" {
		s += " reason=" + c.Reason
	}
	return s
}

// PauseAll is a convenience for a fleet-wide containment (operator use): it
// contains each listed agent and returns every outcome.
func (d *Detector) ContainMany(agents []string, reason string) []Containment {
	out := make([]Containment, 0, len(agents))
	for _, a := range agents {
		out = append(out, d.Contain(a, reason))
	}
	return out
}

// NowString is the detector clock in the wire format, for handlers that need
// "when did the operator ask".
func (d *Detector) NowString() string { return d.now().UTC().Format(time.RFC3339Nano) }
