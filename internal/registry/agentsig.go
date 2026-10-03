package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

	// HeaderAgentBodySHA256 opts a request into BODY BINDING (CR-CHAT-027). A
	// client that wants the per-agent signature to cover the request body
	// sends the lowercase hex sha256 of that exact body here and signs the
	// 4-line transcript built by agentSigPayload. The header is OPTIONAL: when
	// it is ABSENT the request verifies over the original 3-line transcript,
	// byte-for-byte as before, so no existing caller changes behaviour. It is
	// omitted for a request with no body (GET/DELETE) — see RemoteStore
	// signRequest — but a caller MAY bind an empty body by sending the sha256
	// of the empty string.
	HeaderAgentBodySHA256 = "X-Agent-Body-SHA256"
)

// sigWindow is the maximum allowed skew between the client timestamp and
// server time. Bounded replay protection: a captured request is only valid
// for sigWindow seconds.
const sigWindow = 30 * time.Second

// Named outcomes of a body-binding failure (CR-CHAT-027). They are distinct
// from "signature verification failed" because they name a different client
// mistake: the caller opted into body binding, so the body it signed and the
// body it delivered are not the same bytes — nothing is wrong with its key.
const (
	// agentSigBodyDigestEmptyError: the header is present but carries no
	// digest — a signing step that produced nothing. Named rather than
	// treated as "absent" so a client that MEANT to bind its body is told
	// the binding did not happen instead of silently falling back to the
	// request-only transcript (same reasoning as DF-CRIER-236's empty
	// X-Agent-Sig).
	agentSigBodyDigestEmptyError = "X-Agent-Body-SHA256 is present but empty — a client opting into body binding must send the lowercase hex sha256 of the request body it signed"

	// agentSigBodyDigestMismatchError: the headline gap this closes. The
	// digest the caller claims (or the body the caller signed) does not match
	// the delivered bytes, so a body swapped between signing and delivery is
	// refused instead of authorized by the still-valid request signature.
	agentSigBodyDigestMismatchError = "body digest mismatch: X-Agent-Body-SHA256 does not match sha256 of the delivered request body (the signed body differs from the delivered body)"

	// agentSigBodyUnreadableError: the body could not be read to compute its
	// digest, so the binding cannot be checked; fail closed.
	agentSigBodyUnreadableError = "X-Agent-Body-SHA256: the request body could not be read to compute its digest"
)

// agentSigPayload builds the canonical transcript the per-agent signature
// covers. With bodyDigest empty it is the ORIGINAL 3-line form
//
//	<METHOD>\n<path>\n<unix-seconds>
//
// byte-for-byte, so every pre-CR-CHAT-027 caller verifies unchanged. With a
// non-empty bodyDigest (the lowercase hex sha256 of the request body) a 4th
// line names the digest:
//
//	<METHOD>\n<path>\n<unix-seconds>\nsha256:<hex>
//
// Client and server build the transcript through this ONE function so the
// wire contract cannot drift between them.
func agentSigPayload(method, path, ts, bodyDigest string) []byte {
	if bodyDigest == "" {
		return []byte(method + "\n" + path + "\n" + ts)
	}
	return []byte(method + "\n" + path + "\n" + ts + "\nsha256:" + bodyDigest)
}

// sha256Hex returns the lowercase hex encoding of sha256(b) — the digest form
// both the transcript and X-Agent-Body-SHA256 use.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

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
// OPT-IN BODY BINDING (CR-CHAT-027): a caller that sends
// X-Agent-Body-SHA256 (the lowercase hex sha256 of the raw request body) must
// sign the extended transcript "<METHOD>\n<path>\n<unix-seconds>\nsha256:<hex>"
// (agentSigPayload). The server recomputes sha256 of the delivered body and
// refuses the request unless (a) it matches the header and (b) the signature
// verifies over the transcript that includes that digest — so a body swapped
// between the signed request and its delivery is rejected with a NAMED outcome
// instead of being authorized by the still-valid request signature. When the
// header is ABSENT the verification is the original 3-line form, unchanged.
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

	payload := agentSigPayload(r.Method, r.URL.Path, tsRaw, "")
	if vals := r.Header.Values(HeaderAgentBodySHA256); len(vals) > 0 {
		// OPT-IN body binding (CR-CHAT-027). The header is present, so the
		// caller claims the signature covers a body digest: compute it from
		// the DELIVERED bytes and require the claim to match before verifying
		// the signature over the extended transcript.
		claimed := strings.ToLower(strings.TrimSpace(strings.Join(vals, ",")))
		if claimed == "" {
			return h.agentSigFail(w, http.StatusUnauthorized, agentSigBodyDigestEmptyError)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return h.agentSigFail(w, http.StatusUnauthorized, agentSigBodyUnreadableError)
		}
		// Restore the body: authorization runs BEFORE the handler's own read
		// (ack/transfer/patch decode from r.Body), so consuming it here would
		// turn a correctly signed request into an empty-body one.
		r.Body = io.NopCloser(bytes.NewReader(body))
		actual := sha256Hex(body)
		if claimed != actual {
			return h.agentSigFail(w, http.StatusUnauthorized, agentSigBodyDigestMismatchError)
		}
		payload = agentSigPayload(r.Method, r.URL.Path, tsRaw, actual)
	}
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
