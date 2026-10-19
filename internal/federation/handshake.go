package federation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const handshakePath = "/fed/handshake/finish"

// HandshakeTranscript has explicit roles and key ids; JSON of this struct is
// the canonical encoding. Method/path domain-separate it from delivery auth.
// Both signatures cover exactly these bytes, not the enclosing wire object.
type HandshakeTranscript struct {
	Method         string `json:"method"`
	Path           string `json:"path"`
	Timestamp      int64  `json:"timestamp"`
	InitiatorID    string `json:"initiator_id"`
	InitiatorKeyID string `json:"initiator_key_id"`
	InitiatorNonce string `json:"initiator_nonce"`
	ResponderID    string `json:"responder_id"`
	ResponderKeyID string `json:"responder_key_id"`
	ResponderNonce string `json:"responder_nonce"`
}

func (t HandshakeTranscript) Payload() []byte { raw, _ := json.Marshal(t); return raw }

type HandshakeStart struct {
	InstanceID string `json:"instance_id"`
	PublicKey  string `json:"public_key"`
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	Timestamp  int64  `json:"timestamp"`
}

type HandshakeProof struct {
	Transcript         HandshakeTranscript `json:"transcript"`
	ResponderSignature string              `json:"responder_signature"`
	InitiatorSignature string              `json:"initiator_signature,omitempty"`
}

type pendingHandshake struct {
	proof   HandshakeProof
	expires time.Time
	used    bool
}

// HandshakeService is bounded, process-local challenge state. Restart drops
// challenges (fail closed). Trust is pinned by operator Create, never TOFU.
type HandshakeService struct {
	Store   *PeerStore
	id      string
	key     ed25519.PrivateKey
	mu      sync.Mutex
	pending map[string]pendingHandshake
	now     func() time.Time
}

func NewHandshakeService(store *PeerStore, id string, key ed25519.PrivateKey) (*HandshakeService, error) {
	if store == nil || id == "" || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("federation: handshake requires a store, instance id and ed25519 key")
	}
	return &HandshakeService{Store: store, id: id, key: append(ed25519.PrivateKey(nil), key...), pending: make(map[string]pendingHandshake), now: time.Now}, nil
}

func nonceValid(n string) bool { b, err := hex.DecodeString(n); return err == nil && len(b) == 32 }

func (h *HandshakeService) Start(req HandshakeStart) (HandshakeProof, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if delta := now.Sub(time.Unix(req.Timestamp, 0)); delta > fedAuthSkew || delta < -fedAuthSkew {
		return HandshakeProof{}, errors.New("FED_HANDSHAKE_EXPIRED")
	}
	if !nonceValid(req.Nonce) {
		return HandshakeProof{}, errors.New("FED_HANDSHAKE_NONCE_INVALID")
	}
	if req.InstanceID == h.id {
		return HandshakeProof{}, errors.New("FED_HANDSHAKE_SELF")
	}
	p, ok := h.Store.Get(req.InstanceID)
	if !ok {
		return HandshakeProof{}, errors.New(string(FedNoLink))
	}
	if p.State == "revoked" {
		return HandshakeProof{}, errors.New(string(FedPeerRevoked))
	}
	key, err := parseEd25519PublicKey(req.PublicKey)
	if err != nil || hex.EncodeToString(key) != p.PublicKey || req.KeyID != p.KeyID {
		return HandshakeProof{}, errors.New(string(FedBadSignature))
	}
	for n, pending := range h.pending {
		if !now.Before(pending.expires) {
			delete(h.pending, n)
			continue
		}
		if pending.proof.Transcript.InitiatorID == req.InstanceID && pending.proof.Transcript.InitiatorNonce == req.Nonce {
			return HandshakeProof{}, errors.New("FED_HANDSHAKE_REPLAY")
		}
	}
	if len(h.pending) >= 4096 {
		return HandshakeProof{}, errors.New("FED_HANDSHAKE_BUSY")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return HandshakeProof{}, err
	}
	t := HandshakeTranscript{Method: http.MethodPost, Path: handshakePath, Timestamp: req.Timestamp,
		InitiatorID: req.InstanceID, InitiatorKeyID: req.KeyID, InitiatorNonce: req.Nonce,
		ResponderID: h.id, ResponderKeyID: KeyID(h.key.Public().(ed25519.PublicKey)), ResponderNonce: hex.EncodeToString(nonce)}
	proof := HandshakeProof{Transcript: t, ResponderSignature: hex.EncodeToString(ed25519.Sign(h.key, t.Payload()))}
	h.pending[t.ResponderNonce] = pendingHandshake{proof: proof, expires: now.Add(fedAuthSkew)}
	return proof, nil
}

func (h *HandshakeService) Finish(proof HandshakeProof) (PeerRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := proof.Transcript
	pending, ok := h.pending[t.ResponderNonce]
	if !ok {
		return PeerRecord{}, errors.New("FED_HANDSHAKE_OUT_OF_ORDER")
	}
	if pending.used {
		return PeerRecord{}, errors.New("FED_HANDSHAKE_REPLAY")
	}
	now := h.now()
	if !now.Before(pending.expires) || now.Sub(time.Unix(t.Timestamp, 0)) > fedAuthSkew || time.Unix(t.Timestamp, 0).Sub(now) > fedAuthSkew {
		return PeerRecord{}, errors.New("FED_HANDSHAKE_EXPIRED")
	}
	if t != pending.proof.Transcript {
		return PeerRecord{}, errors.New("FED_HANDSHAKE_NONCE_MISMATCH")
	}
	// Consume before signature validation: a failed guess cannot reuse a nonce.
	pending.used = true
	h.pending[t.ResponderNonce] = pending
	p, ok := h.Store.Get(t.InitiatorID)
	if !ok {
		return PeerRecord{}, errors.New(string(FedNoLink))
	}
	if p.State == "revoked" {
		return PeerRecord{}, errors.New(string(FedPeerRevoked))
	}
	key, err := parseEd25519PublicKey(p.PublicKey)
	if err != nil || p.KeyID != t.InitiatorKeyID {
		return PeerRecord{}, errors.New(string(FedBadSignature))
	}
	sig, err := hex.DecodeString(proof.InitiatorSignature)
	responderSig, rerr := hex.DecodeString(proof.ResponderSignature)
	if err != nil || rerr != nil || !ed25519.Verify(key, t.Payload(), sig) || !ed25519.Verify(h.key.Public().(ed25519.PublicKey), t.Payload(), responderSig) {
		return PeerRecord{}, errors.New(string(FedBadSignature))
	}
	return h.Store.link(p.ID, p.KeyID)
}

// OperatorGate is fail-closed even when the ordinary deployment gate is off.
// It does not bypass that gate: callers must still satisfy every existing check.
func OperatorGate(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential := r.Header.Get("Authorization")
		if operator := r.Header.Get("X-Crier-Fed-Operator"); operator != "" {
			credential = BearerPrefix + operator
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(credential), []byte(BearerPrefix+token)) != 1 {
			fedJSON(w, http.StatusForbidden, map[string]string{"error": "FED_OPERATOR_REQUIRED"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeFed(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fedJSON(w, 400, map[string]string{"error": "FED_INVALID_REQUEST"})
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fedJSON(w, 400, map[string]string{"error": "FED_INVALID_REQUEST"})
		return false
	}
	return true
}

func fedJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *HandshakeService) HandleStart(w http.ResponseWriter, r *http.Request) {
	var req HandshakeStart
	if !decodeFed(w, r, &req) {
		return
	}
	proof, err := h.Start(req)
	if err != nil {
		fedJSON(w, 403, map[string]string{"error": err.Error()})
		return
	}
	fedJSON(w, 200, proof)
}
func (h *HandshakeService) HandleFinish(w http.ResponseWriter, r *http.Request) {
	var proof HandshakeProof
	if !decodeFed(w, r, &proof) {
		return
	}
	p, err := h.Finish(proof)
	if err != nil {
		fedJSON(w, 403, map[string]string{"error": err.Error()})
		return
	}
	fedJSON(w, 200, p)
}
func (s *PeerStore) HandleList(w http.ResponseWriter, r *http.Request) {
	fedJSON(w, 200, map[string]any{"peers": s.List()})
}
func (s *PeerStore) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string `json:"id"`
		PublicKey string `json:"public_key"`
		URL       string `json:"url"`
		Direction string `json:"direction"`
		Label     string `json:"label"`
	}
	if !decodeFed(w, r, &req) {
		return
	}
	p, err := s.Create(PeerRecord{ID: req.ID, PublicKey: req.PublicKey, URL: req.URL, Direction: req.Direction, Label: req.Label})
	if err != nil {
		fedJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	fedJSON(w, 201, p)
}
func (s *PeerStore) HandlePatch(id string, w http.ResponseWriter, r *http.Request) {
	var patch PeerPatch
	if !decodeFed(w, r, &patch) {
		return
	}
	p, err := s.Patch(id, patch)
	if err != nil {
		fedJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	fedJSON(w, 200, p)
}

// VerifyDelivery resolves the lifecycle record at the crossing, never caches
// admission, and keeps the legacy per-request transcript/signature check.
func (s *PeerStore) VerifyDelivery(r *http.Request) Verdict {
	peer := r.Header.Get(PeerHeader)
	if peer == "" {
		return FedNoLink
	}
	p, found := s.Get(peer)
	if v := peerVerdict(p, found); v != FedAllowed {
		return v
	}
	key, err := parseEd25519PublicKey(p.PublicKey)
	if err != nil || r.Header.Get("X-Fed-Key-Id") != p.KeyID {
		return FedBadSignature
	}
	if verifyPeerSig(key, peer, r.Method, r.URL.Path, r.Header.Get(FedAuthTsHeader), r.Header.Get(FedAuthSigHeader)) != nil {
		return FedBadSignature
	}
	// Bind the complete payload as well as the legacy method/path/timestamp.
	// The body is restored for the existing delivery decoder.
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return FedBadSignature
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	sig, err := hex.DecodeString(r.Header.Get("X-Fed-Body-Sig"))
	if err != nil || !ed25519.Verify(key, deliveryPayload(r, body), sig) {
		return FedBadSignature
	}
	return FedAllowed
}

func deliveryPayload(r *http.Request, body []byte) []byte {
	prefix := "crier-fed-delivery-v1\n" + r.Header.Get(PeerHeader) + "\n" + r.Header.Get("X-Fed-Key-Id") + "\n" + string(FedAuthPayload(r.Method, r.URL.Path, r.Header.Get(FedAuthTsHeader))) + "\n" + strconv.Itoa(len(body)) + "\n"
	return append([]byte(prefix), body...)
}
