package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Per-agent request signing headers.
const (
	HeaderAgentID  = "X-Agent-ID"
	HeaderAgentTS  = "X-Agent-Ts"
	HeaderAgentSig = "X-Agent-Sig"
)

// sigWindow is the maximum allowed skew between the client timestamp and
// server time. Bounded replay protection: a captured request is only valid
// for sigWindow seconds.
const sigWindow = 30 * time.Second

// agentSigError wraps an authorization failure with the HTTP status to return.
type agentSigError struct {
	status  int
	message string
}

func (e *agentSigError) Error() string { return e.message }

// authorizeAgent verifies that the caller proves possession of the private
// key corresponding to the target agent's registered ed25519 public key.
//
// Signed payload: "<METHOD>\n<path>\n<unix-seconds>" where path is the raw
// URL path (query string excluded). Headers:
//
//	X-Agent-ID:  the agent claiming to act
//	X-Agent-Ts:  unix timestamp (seconds), must be within ±30s of server time
//	X-Agent-Sig: hex-encoded ed25519 signature over the payload
//
// The caller's X-Agent-ID must match the target agent in the URL — an agent
// can only read/ack/delete its own inbox. Callers that lack a registered key
// (or present a mismatched key) are rejected with 401/403.
//
// It writes the error response and returns a non-nil error when unauthorized.
func (h *Handler) authorizeAgent(w http.ResponseWriter, r *http.Request, targetID string) error {
	callerID := r.Header.Get(HeaderAgentID)
	tsRaw := r.Header.Get(HeaderAgentTS)
	sigRaw := r.Header.Get(HeaderAgentSig)

	if callerID == "" || tsRaw == "" || sigRaw == "" {
		return h.agentSigFail(w, http.StatusUnauthorized,
			"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)")
	}

	// Resolve the target agent first. An unknown target returns 404 — no
	// existence leak regardless of who is calling.
	agent, err := h.store.Get(targetID)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			return h.agentSigFail(w, http.StatusNotFound, "agent not found")
		}
		writeStoreError(w, err)
		return &agentSigError{status: http.StatusInternalServerError, message: "store error"}
	}

	if callerID != targetID {
		return h.agentSigFail(w, http.StatusForbidden,
			fmt.Sprintf("agent %q may only access its own resources (target %q)", callerID, targetID))
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return h.agentSigFail(w, http.StatusUnauthorized, "X-Agent-Ts must be a unix timestamp in seconds")
	}
	skew := time.Since(time.Unix(ts, 0))
	if skew > sigWindow || skew < -sigWindow {
		return h.agentSigFail(w, http.StatusUnauthorized,
			fmt.Sprintf("request timestamp outside allowed window (±%s)", sigWindow))
	}

	rawKey := ed25519.PublicKey(agent.PublicKey)
	if len(rawKey) != ed25519.PublicKeySize {
		return h.agentSigFail(w, http.StatusInternalServerError, "agent has an invalid stored public key")
	}

	sig, err := hex.DecodeString(sigRaw)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return h.agentSigFail(w, http.StatusUnauthorized, "X-Agent-Sig must be hex-encoded ed25519 signature (128 hex chars)")
	}

	payload := []byte(r.Method + "\n" + r.URL.Path + "\n" + tsRaw)
	if !ed25519.Verify(rawKey, payload, sig) {
		return h.agentSigFail(w, http.StatusUnauthorized, "signature verification failed")
	}

	return nil
}

func (h *Handler) agentSigFail(w http.ResponseWriter, status int, message string) error {
	writeJSON(w, status, map[string]string{"error": message})
	return &agentSigError{status: status, message: message}
}

// requireAgent writes the authorization error if per-agent signing is enabled
// and the request is not properly signed for targetID.
func (h *Handler) requireAgent(w http.ResponseWriter, r *http.Request, targetID string) bool {
	if !h.requireAgentSig {
		return true
	}
	return h.authorizeAgent(w, r, targetID) == nil
}
