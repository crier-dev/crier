package session

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/crier-dev/crier/internal/registry"
)

const (
	SenderIdentityClaimed  = "sender-identity-claimed"
	SenderIdentityVerified = "sender-identity-verified"
)

// decodeSendBody preserves the exact bytes for the registry's optional body
// digest binding. Decoding/re-encoding JSON is not a signature-preserving read.
func decodeSendBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSessionBodyBytes+1))
	if err != nil || len(body) > maxSessionBodyBytes {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "could not read a session request within the 1 MiB limit")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	ok := decodeBody(w, r, dst)
	r.Body = io.NopCloser(bytes.NewReader(body))
	return ok
}

// authorizeSender runs before request-key replay, seq allocation, branching or
// dispatch. A signed key authenticates an agent, NOT a human principal/binding.
// DF-CRIER-333 owns browser authentication; this does not invent one.
func (h *Handler) authorizeSender(w http.ResponseWriter, r *http.Request, author AuthorRef) (string, bool) {
	offered := false
	for _, header := range []string{registry.HeaderAgentSig, registry.HeaderAgentTS, registry.HeaderAgentBodySHA256} {
		offered = offered || len(r.Header.Values(header)) > 0
	}
	if h.opts.RequireAgentSignature && !offered {
		writeAPIError(w, http.StatusUnauthorized, "SESSION_SENDER_SIGNATURE_REQUIRED", "this deployment requires a signed agent request; this browser cannot sign. No message was recorded. Browser authentication remains an open owner decision (DF-CRIER-333)")
		return "", false
	}
	if !offered && !h.opts.RequireAgentSignature {
		return SenderIdentityClaimed, true
	}
	if h.opts.AuthorizeSender == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "SESSION_SENDER_SIGNATURE_UNAVAILABLE", "signature verifier is not configured; no message was recorded")
		return "", false
	}
	verified, allowed := h.opts.AuthorizeSender(senderRefusalWriter{w}, r, h.opts.RequireAgentSignature)
	if !allowed {
		return "", false
	}
	if !verified {
		writeAPIError(w, http.StatusUnauthorized, "SESSION_SENDER_SIGNATURE_INVALID", "the signature verifier returned no proof; no message was recorded")
		return "", false
	}
	if author.Principal != "" || author.Agent == "" || author.Agent != strings.TrimSpace(r.Header.Get(registry.HeaderAgentID)) {
		writeAPIError(w, http.StatusForbidden, "SESSION_SENDER_IDENTITY_MISMATCH", "the signed agent must match the message sender; an agent key does not authenticate a principal or an as_agent binding. No message was recorded")
		return "", false
	}
	return SenderIdentityVerified, true
}

// Reuse the registry's verifier/status without publishing raw malformed proof
// bytes in a transcript/page. All its refusals have a stable session code.
type senderRefusalWriter struct{ http.ResponseWriter }

func (w senderRefusalWriter) Write(body []byte) (int, error) {
	replacement, _ := json.Marshal(map[string]string{"error": "SESSION_SENDER_SIGNATURE_INVALID", "detail": "agent request signature could not be verified; check the registered key, signing headers, timestamp and any body digest. No message was recorded"})
	_, err := w.ResponseWriter.Write(append(replacement, '\n'))
	if err != nil {
		return 0, err
	}
	return len(body), nil
}
