package registry

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/trust"
)

// TrustHandler manages verifier-local records only. Its operator credential is
// the existing CR_PERMISSIONS_ADMIN_TOKEN, independent of ACL enablement.
// The message/reach credential can verify trust but cannot read/write records.
type TrustHandler struct {
	store         TrustStore
	operatorToken string
}

func NewTrustHandler(store Store, operatorToken, reachToken string) *TrustHandler {
	// Preserve existing configuration/auth semantics, but fail this NEW gate
	// closed when the two roles collapse (the ACL-off config allows that).
	if operatorToken == reachToken {
		operatorToken = ""
	}
	backend, _ := store.(TrustStore)
	return &TrustHandler{store: backend, operatorToken: operatorToken}
}
func (h *TrustHandler) operator(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if h.operatorToken == "" || !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(h.operatorToken)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "MANAGEMENT_FORBIDDEN", "detail": "trust records require a distinct CR_PERMISSIONS_ADMIN_TOKEN"})
		return false
	}
	return h.available(w)
}
func (h *TrustHandler) available(w http.ResponseWriter) bool {
	if h.store == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "TRUST_UNSUPPORTED"})
		return false
	}
	return true
}
func decodeTrustBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "TRUST_MALFORMED", "detail": err.Error()})
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "TRUST_MALFORMED", "detail": "expected exactly one JSON object"})
		return false
	}
	return true
}
func trustStoreError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "store failure"
	switch {
	case errors.Is(err, ErrInvalidStoreInput):
		status = 400
		code = "TRUST_MALFORMED"
	case errors.Is(err, ErrTrustExists):
		status = 409
		code = "TRUST_EXISTS"
	case errors.Is(err, ErrTrustUnsupported):
		status = 501
		code = "TRUST_UNSUPPORTED"
	}
	detail := code
	if status != 500 {
		detail = err.Error()
	}
	writeJSON(w, status, map[string]string{"error": code, "detail": detail})
}
func (h *TrustHandler) HandleCreateAnchor(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var a trust.ChainAnchor
	if !decodeTrustBody(w, r, &a) {
		return
	}
	// The operator decides trust; the server records when that decision landed.
	a.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := h.store.AddTrustAnchor(a); err != nil {
		trustStoreError(w, err)
		return
	}
	writeJSON(w, 201, a)
}
func (h *TrustHandler) HandleListAnchors(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	anchors, err := h.store.ListTrustAnchors()
	if err != nil {
		trustStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"anchors": anchors})
}
func (h *TrustHandler) HandleCreateDelegation(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	var d trust.ChainDelegation
	if !decodeTrustBody(w, r, &d) {
		return
	}
	if err := h.store.AddTrustDelegation(d); err != nil {
		trustStoreError(w, err)
		return
	}
	writeJSON(w, 201, d)
}
func (h *TrustHandler) HandleListDelegations(w http.ResponseWriter, r *http.Request) {
	if !h.operator(w, r) {
		return
	}
	subject := r.URL.Query().Get("subject")
	raw, err := hex.DecodeString(subject)
	if err != nil || len(raw) != 32 {
		writeJSON(w, 400, map[string]string{"error": "TRUST_MALFORMED", "detail": "subject must be a 32-byte hex public key"})
		return
	}
	records, err := h.store.ListTrustDelegations(subject)
	if err != nil {
		trustStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"delegations": records})
}
func (h *TrustHandler) HandleVerify(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	var req struct {
		Signer string                  `json:"signer"`
		Scope  []string                `json:"scope"`
		Chain  []trust.ChainDelegation `json:"chain"`
	}
	if !decodeTrustBody(w, r, &req) {
		return
	}
	if req.Scope == nil || req.Chain == nil {
		writeJSON(w, 400, map[string]string{"error": "TRUST_MALFORMED", "detail": "scope and chain must be arrays (use [] for empty)"})
		return
	}
	if err := trust.ValidateChainRequest(req.Signer, req.Scope, req.Chain); err != nil {
		writeJSON(w, 400, map[string]string{"error": "TRUST_MALFORMED", "detail": err.Error()})
		return
	}
	anchors, err := h.store.ListTrustAnchors()
	if err != nil {
		trustStoreError(w, err)
		return
	}
	writeJSON(w, 200, trust.VerifyChain(anchors, req.Signer, req.Scope, req.Chain))
}
