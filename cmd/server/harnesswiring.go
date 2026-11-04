// harnesswiring.go — the joint between the push-ingress surface and the
// harness adapters (CRIER-INGRESS-1 hook × CRIER-HARNESS-1 registry).
//
// The two rows were landed separately by design: ingress left
// Handler.Dispatch nil so its `delivery` object honestly reported
// {"mode":"store","state":"stored"}, and the harness layer exported
// Registry.Deliver as "the hook COURIER-INGRESS §4.2 calls". This file closes
// the loop and nothing more:
//
//   - it reads the harness binding recorded on the courier session
//     (Session.Harness / Session.HarnessSessionID),
//   - refuses to dispatch a session with no binding (store-only, which is the
//     honest report, never a fake `dispatched`),
//   - passes the message text as ONE field (the adapters build argv; nothing
//     here ever reaches a shell),
//   - maps the harness Receipt onto ingress's DeliveryReport WITHOUT ever
//     turning a failed delivery into a failed HTTP call: the message is
//     already durable, so a harness failure is reported inside a 2xx.
//
// LIFECYCLE NOTE, stated rather than hidden: the first delivery to a session
// that names a harness but has no HarnessSessionID yet CREATES the harness
// session, and the adapter's receipt carries the id it minted. Recording that
// id back onto the courier session is a store write this file does not do
// (it would need the session store's append path and a new record kind), so a
// session created this way re-uses the id only for the lifetime of the
// process: the registry's receipt is returned to the caller, and persisting
// the binding is the follow-up row this comment names. A session that ALREADY
// carries HarnessSessionID — the state a deployment reaches by recording the
// binding once — continues correctly with no ambiguity.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/harness"
	harnessbuild "github.com/crier-dev/crier/internal/harness/build"
	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

// harnessDispatch implements ingress.DeliveryDispatch over a built registry.
type harnessDispatch struct {
	reg  *harness.Registry
	repo session.Repository
}

var _ ingress.DeliveryDispatch = (*harnessDispatch)(nil)

// Dispatch offers one stored message to the adapter the session is bound to.
//
// An error return is reserved for "the dispatch could not be attempted" —
// ingress renders it as delivery.state=failed on an otherwise successful call.
// A harness-side failure arrives as a Receipt with State=failed and is mapped
// to the same report.
func (d *harnessDispatch) Dispatch(ctx context.Context, req ingress.DispatchRequest) (ingress.DeliveryReport, error) {
	stored := ingress.DeliveryReport{Mode: "store", State: "stored"}

	// An inbox push has no courier session to bind to, and a message with no
	// session id cannot be serialised per session; both stay store-only.
	if d == nil || d.reg == nil || !d.reg.Enabled() || strings.TrimSpace(req.SessionID) == "" {
		return stored, nil
	}

	st, err := d.repo.Load(ctx, req.SessionID)
	if err != nil || st == nil || st.Session == nil {
		// The message is durable and the route already proved the session
		// exists; failing to re-read it here is a store inconsistency worth
		// reporting, but it must not fail the call.
		return ingress.DeliveryReport{Mode: "store", State: "stored",
			Error: fmt.Sprintf("harness dispatch skipped: session %q could not be re-read: %v", req.SessionID, err)}, nil
	}

	name := strings.TrimSpace(st.Session.Harness)
	if name == "" {
		return stored, nil // store-only session: nothing to claim it
	}

	receipt := d.reg.Deliver(ctx, harness.DispatchRequest{
		Harness: name,
		SessionRef: harness.SessionRef{
			Harness: name,
			ID:      st.Session.HarnessSessionID,
		},
		Message: harness.Message{
			ID:        req.MessageID,
			Text:      req.Text,
			Tags:      req.Tags,
			Author:    req.Author.AgentID(),
			SessionID: req.SessionID,
		},
	})

	// COURIER-HARNESS §4.3: the FIRST successful delivery to a session that
	// named a harness but had no id records the id the adapter minted, so every
	// later message continues THAT session instead of creating a second one.
	// Until this lands, a binding lived only for the process lifetime.
	if receipt.State == harness.StateDispatched && receipt.SessionID != "" && st.Session.HarnessSessionID == "" {
		if seq, err := d.repo.NextSeq(ctx, req.SessionID); err == nil {
			rec := session.HarnessBindRecord(req.SessionID, seq, time.Now().UTC(), name, receipt.SessionID)
			if err := d.repo.Append(ctx, rec); err != nil {
				// The message IS delivered; failing the call would be a lie about
				// the transcript. Report the bind problem in the delivery object
				// so an operator sees it instead of a silent re-create next time.
				slog.Warn("harness bind record failed", "session", req.SessionID, "harness", name, "err", err)
			}
		}
	}

	report := ingress.DeliveryReport{
		Mode:    "harness",
		Harness: receipt.Harness,
		State:   receipt.State,
	}
	if receipt.Error != "" {
		report.Error = receipt.Error
		if receipt.Tail != "" {
			report.Error = receipt.Error + ": " + firstLine(receipt.Tail)
		}
	}
	// A receipt that names neither state nor error is not a success claim: say
	// stored rather than inventing `dispatched`.
	if report.State == "" {
		report.State = "stored"
	}
	return report, nil
}

// firstLine keeps a receipt's stderr tail to its first line so the delivery
// object stays a status, not a log.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// buildHarnessRegistry builds the adapter registry from the process config.
// It is the same construction the `crier harness` CLI verbs use, so an operator
// who can list an adapter can also have it deliver. A build failure is logged
// and yields nil rather than failing startup: no harness config can prevent the
// daemon from serving the store-only surface.
func buildHarnessRegistry(cfg config.Config) *harness.Registry {
	reg := harnessbuild.Registry(cfg.Harness)
	if reg == nil {
		return nil
	}
	for _, w := range cfg.Harness.Warnings {
		slog.Warn("harness config", "reason", w)
	}
	return reg
}
