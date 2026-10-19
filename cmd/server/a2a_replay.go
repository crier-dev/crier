package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

// sendOnce reserves in the same backend that owns the inbox, then dispatches
// through the existing handler. No database write lock spans external delivery.
func (h *a2aHandler) sendOnce(agentID string, tr *a2a.Translation, ctx context.Context) (*a2a.Replay, *a2a.RPCError) {
	fail := func(err error) (*a2a.Replay, *a2a.RPCError) {
		return nil, a2a.NewRPCError(a2a.CodeInternalError, err.Error())
	}
	store, ok := h.store.(registry.A2AReplayStore)
	if !ok {
		return fail(fmt.Errorf("A2A replay storage unsupported by registry backend"))
	}
	key := tr.Request.IdempotencyKey
	token, raw, err := h.acquireA2AReplay(ctx, store, agentID, key)
	if err != nil {
		return fail(err)
	}
	if raw != nil {
		receipt, err := a2a.DecodeReplay(raw)
		if err != nil {
			return fail(fmt.Errorf("read A2A replay receipt: %w", err))
		}
		receipt.Accept.IdempotentReplay = true
		slog.Info("A2A send replayed existing task", "agent_id", agentID, "message_id", key, "task_id", receipt.Accept.ID, "idempotent_replay", true)
		return receipt, nil
	}
	status, body, err := h.deliverTo(agentID, tr.Request, ctx)
	if err != nil {
		return fail(err)
	} // ambiguous: retain reservation, never re-dispatch
	if status < 200 || status > 299 {
		if status < 500 {
			if err := store.FinishA2AReplay(agentID, key, token, nil); err != nil {
				return fail(fmt.Errorf("release A2A rejection: %w", err))
			}
		}
		return nil, a2a.ErrorFromStatus(status, body)
	}
	receipt := &a2a.Replay{Translation: tr, AcceptedAt: h.opts.now()}
	if err := json.Unmarshal(body, &receipt.Accept); err != nil {
		return fail(fmt.Errorf("read A2A delivery accept: %w", err))
	}
	if receipt.Accept.ID == "" {
		return fail(fmt.Errorf("A2A delivery accept has no task id"))
	}
	raw, err = json.Marshal(receipt)
	if err != nil {
		return fail(fmt.Errorf("encode A2A replay receipt: %w", err))
	}
	// Persistence is mandatory BEFORE returning success or opening a stream.
	if err := store.FinishA2AReplay(agentID, key, token, raw); err != nil {
		return fail(fmt.Errorf("persist A2A replay receipt (delivery may have succeeded): %w", err))
	}
	return receipt, nil
}

// Preserve the existing registry retry behavior: concurrent sends can wait up
// to two seconds for the first accept. Durable pending state is never stolen.
func (h *a2aHandler) acquireA2AReplay(ctx context.Context, store registry.A2AReplayStore, agentID, key string) (string, []byte, error) {
	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		token, raw, err := store.AcquireA2AReplay(agentID, key, h.opts.now(), h.opts.replayWindow)
		if !errors.Is(err, registry.ErrA2AReplayPending) {
			return token, raw, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return "", nil, err
		case <-timer.C:
		}
	}
}
