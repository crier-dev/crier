package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
// The header check distinguishes three separate client mistakes instead of
// collapsing them into one message (DF-CRIER-236): a header that is ABSENT
// ("missing agent signature headers"), an X-Agent-Sig that is PRESENT but
// empty or whitespace-only — the signature of a signing helper that produced
// no output (openssl pkeyutl -sign -rawin needs a seekable payload supplied
// with -in <file>; a piped or redirected payload fails and yields zero bytes,
// and the helper's discarded stderr hid it) — and an X-Agent-Sig that is
// present but malformed (not hex, or hex of the wrong length). All three fail
// closed with 401; only the message differs, so a caller debugs the real
// cause instead of the headers.
//
// It writes the error response and returns a non-nil error when unauthorized.
func (h *Handler) authorizeAgent(w http.ResponseWriter, r *http.Request, targetID string) error {
	callerID := r.Header.Get(HeaderAgentID)
	tsRaw := r.Header.Get(HeaderAgentTS)
	sigRaw := r.Header.Get(HeaderAgentSig)

	if callerID == "" || tsRaw == "" {
		return h.agentSigFail(w, http.StatusUnauthorized,
			"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)")
	}

	// X-Agent-ID and X-Agent-Ts are here, so a blank X-Agent-Sig is not a
	// missing header — it is a signing step that produced no output. Name it,
	// and name the cause, because the empty string is what a non-seekable
	// payload looks like from the client side (DF-CRIER-236).
	if strings.TrimSpace(sigRaw) == "" {
		return h.agentSigFail(w, http.StatusUnauthorized,
			"X-Agent-Sig is present but empty — the client's signing step produced no output. "+
				"openssl pkeyutl -sign -rawin needs OpenSSL >= 3 AND a seekable payload passed with -in <file>: "+
				"a piped or redirected payload fails with \"unable to determine file size for oneshot operation\" "+
				"and yields a zero-byte signature.")
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
		// Fail closed (DF-CRIER-192): a stored key that is not a valid
		// 32-byte ed25519 key — in practice a keyless agent, registered
		// while signature enforcement was off — can never satisfy
		// signature verification. That is an authorization failure of
		// this request (401, naming the real reason), not a server
		// error: it must never 500, and never 200.
		return h.agentSigFail(w, http.StatusUnauthorized,
			"agent has no registered public key (registered without a key; signature verification is impossible)")
	}

	sig, err := hex.DecodeString(sigRaw)
	if err != nil || len(sig) != ed25519.SignatureSize {
		// Two different client bugs, two different messages (DF-CRIER-236):
		// rubbish that is not hex at all, and hex of the wrong length. The
		// reject decision is unchanged — only the message is specific.
		if !isHexSignature(sigRaw) {
			return h.agentSigFail(w, http.StatusUnauthorized,
				fmt.Sprintf("X-Agent-Sig is malformed: %q is not hex "+
					"(expected the hex encoding of a 64-byte ed25519 signature)", sigRaw))
		}
		return h.agentSigFail(w, http.StatusUnauthorized,
			fmt.Sprintf("X-Agent-Sig is malformed: expected 128 hex chars "+
				"(64-byte ed25519 signature), got %d character(s) (%q)", len(sigRaw), sigRaw))
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

// isHexSignature reports whether s is non-empty and made only of hexadecimal
// digits. It exists so authorizeAgent can tell "the client sent something that
// is not hex at all" (isHexSignature false) apart from "the client sent hex of
// the wrong length" — two different client bugs that used to share one
// message. Uppercase hex is accepted, matching encoding/hex.
func isHexSignature(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// requireAgent writes the authorization error if per-agent signing is enabled
// and the request is not properly signed for targetID.
func (h *Handler) requireAgent(w http.ResponseWriter, r *http.Request, targetID string) bool {
	if !h.requireAgentSig {
		return true
	}
	return h.authorizeAgent(w, r, targetID) == nil
}
