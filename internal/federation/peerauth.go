package federation

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Federation peer authentication (CR-CHAT-024, specs/FEDERATION-AUTH.md).
//
// Before this file, a federation request that announced a peer identity
// (PeerHeader, CR-CHAT-023) announced a CLAIM — the destination's policy
// consulted the named peer's record, but nothing proved the request actually
// came from that peer. This layer makes the claim verifiable: each peer has a
// stable ed25519 identity (same crypto family as the per-agent signatures in
// internal/registry/agentsig.go — no new crypto), and a request that announces
// a peer identity must carry a signature over a canonical transcript that
// only the holder of that peer's private key can produce.
//
// Everything here is CONFIG-GATED and additive: when CR_FED_AUTH_FILE is
// unset, no federation request is ever asked for a signature and the shipped
// shared-secret posture (CR_AUTH_TOKEN / CR_FED_TOKEN Bearer, DF-CRIER-6)
// behaves byte-for-byte as before. When it is set, a request carrying
// X-Crier-Fed-Peer must ALSO carry valid X-Fed-Ts + X-Fed-Sig headers; a
// request without the peer announcement is unaffected.

// Federation peer-auth request headers (CR-CHAT-024).
const (
	// FedAuthTsHeader carries the unix timestamp (seconds) the signature
	// covers — the replay window anchor, like X-Agent-Ts for agents.
	FedAuthTsHeader = "X-Fed-Ts"
	// FedAuthSigHeader carries the hex-encoded ed25519 signature over the
	// transcript built by FedAuthPayload.
	FedAuthSigHeader = "X-Fed-Sig"
)

// fedAuthSkew is the maximum allowed skew between the signed timestamp and
// server time — the same bounded-replay window the per-agent signatures use.
const fedAuthSkew = 30 * time.Second

// FedAuthPayload builds the canonical transcript a federation peer signature
// covers:
//
//	<METHOD>\n<path>\n<unix-seconds>
//
// path is the raw URL path, query string excluded — the same transcript shape
// the per-agent signature (agentSigPayload) uses, so operators reason about
// one signing scheme across crier. Client and server MUST build it through
// this one function so the wire contract cannot drift.
func FedAuthPayload(method, path, ts string) []byte {
	return []byte(method + "\n" + path + "\n" + ts)
}

// PeerIdentity is one configured peer's stable identity: its local peer id
// (the policy key CR-CHAT-023 resolves) and its ed25519 public key in hex.
type PeerIdentity struct {
	Peer      string `json:"peer"`
	PublicKey string `json:"public_key"`
}

// peerAuthDocument is the CR_FED_AUTH_FILE document shape.
type peerAuthDocument struct {
	Peers []PeerIdentity `json:"peers"`
	// Revoked lists peer ids that must be refused even if a peer record
	// (or a stale cache) still exists — revocation wins over registration.
	Revoked []string `json:"revoked"`
}

// PeerAuthError is a federation peer-auth failure with the HTTP status the
// caller must answer. 401 means the request did not prove the identity it
// claimed (unknown peer, bad timestamp, bad signature); 403 means the
// identity was proven but is REVOKED — an operator decision, not a proof
// failure.
type PeerAuthError struct {
	Status  int
	Message string
}

func (e *PeerAuthError) Error() string { return e.Message }

// PeerAuth is the peer-identity + revocation state loaded from
// CR_FED_AUTH_FILE. It re-reads the document when the file's mtime or size
// changes, so an operator can revoke a peer (or register a new one) by
// editing the file — no code change, no restart.
type PeerAuth struct {
	mu      sync.Mutex
	path    string
	peers   map[string]ed25519.PublicKey
	revoked map[string]bool
	modTime time.Time
	size    int64
	loaded  bool
}

// LoadPeerAuthFile reads and validates the peer-auth document at path.
// Every peer record must carry a peer id and a well-formed 32-byte hex
// ed25519 public key; a bad record is a load error, never a silently
// skipped peer (an operator's typo must not look like a revoked peer).
func LoadPeerAuthFile(path string) (*PeerAuth, error) {
	a := &PeerAuth{path: path}
	if err := a.reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// reload re-reads the document. Caller holds a.mu.
func (a *PeerAuth) reload() error {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if a.loaded {
			// A transient read failure on an already-loaded state must not
			// tear down the last known-good peer set; the stat check below
			// retries on the next request.
			return nil
		}
		return fmt.Errorf("federation: read peer auth file: %w", err)
	}
	peers, revoked, err := parsePeerAuthDocument(raw, a.path)
	if err != nil {
		if a.loaded {
			slog.Error("federation peer auth file update is invalid; keeping last known-good state", "path", a.path, "error", err)
			return nil
		}
		return err
	}
	// Record the file identity of the bytes we just parsed so refresh()
	// can detect a later change.
	if st, serr := os.Stat(a.path); serr == nil {
		a.modTime = st.ModTime()
		a.size = st.Size()
	}
	a.peers = peers
	a.revoked = revoked
	a.loaded = true
	return nil
}

// parsePeerAuthDocument validates raw and returns the peer map + revocation
// set. Split out of reload so both the initial load and updates validate
// identically.
func parsePeerAuthDocument(raw []byte, path string) (peers map[string]ed25519.PublicKey, revoked map[string]bool, err error) {
	var doc peerAuthDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("federation: parse peer auth file %s: %w", path, err)
	}
	peers = make(map[string]ed25519.PublicKey)
	for i, p := range doc.Peers {
		id := strings.TrimSpace(p.Peer)
		if id == "" {
			return nil, nil, fmt.Errorf("federation: peer auth file %s: peers[%d] has no peer id", path, i)
		}
		if _, dup := peers[id]; dup {
			return nil, nil, fmt.Errorf("federation: peer auth file %s: duplicate peer id %q", path, id)
		}
		key, kerr := parseEd25519PublicKey(p.PublicKey)
		if kerr != nil {
			return nil, nil, fmt.Errorf("federation: peer auth file %s: peer %q: %w", path, id, kerr)
		}
		peers[id] = key
	}
	revoked = make(map[string]bool)
	for _, id := range doc.Revoked {
		revoked[strings.TrimSpace(id)] = true
	}
	return peers, revoked, nil
}

// parseEd25519PublicKey decodes a hex ed25519 public key.
func parseEd25519PublicKey(hexKey string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("public_key is not hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public_key must be %d bytes of hex, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// refresh re-reads the document when the file changed on disk. Best-effort:
// a stat/read failure keeps the last known-good state (recorded above).
func (a *PeerAuth) refresh() {
	st, err := os.Stat(a.path)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded && st.ModTime().Equal(a.modTime) && st.Size() == a.size {
		return
	}
	_ = a.reload()
}

// PublicKeyFor returns the peer's registered public key. The second return
// is false when the peer has no identity record.
func (a *PeerAuth) PublicKeyFor(peer string) (ed25519.PublicKey, bool) {
	if a == nil {
		return nil, false
	}
	a.refresh()
	a.mu.Lock()
	defer a.mu.Unlock()
	key, ok := a.peers[peer]
	return key, ok
}

// IsRevoked reports whether the peer id is on the revocation list.
func (a *PeerAuth) IsRevoked(peer string) bool {
	if a == nil {
		return false
	}
	a.refresh()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revoked[peer]
}

// VerifyPeerSignature checks that a request announcing peer carries a valid
// timestamp + signature for that peer's registered public key. It returns
// nil when the identity is proven; otherwise a *PeerAuthError carrying the
// HTTP status to answer:
//
//   - 401: the peer has no registered identity, the timestamp is missing /
//     malformed / outside the replay window, or the signature is missing /
//     malformed / does not verify;
//   - 403: the signature verified BUT the peer is revoked (FED_PEER_REVOKED)
//     — proof succeeded, admission is an operator decision.
//
// Revocation is checked BEFORE signature work: a revoked peer gets the same
// answer whether or not it holds its key.
func (a *PeerAuth) VerifyPeerSignature(peer, method, path, tsRaw, sigRaw string) *PeerAuthError {
	if a.IsRevoked(peer) {
		return &PeerAuthError{
			Status:  http.StatusForbidden,
			Message: fmt.Sprintf("federation peer %q is revoked (FED_PEER_REVOKED): the operator has disabled this identity", peer),
		}
	}
	key, ok := a.PublicKeyFor(peer)
	if !ok {
		return &PeerAuthError{
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("federation peer %q has no registered identity (no public key record)", peer),
		}
	}
	return verifyPeerSig(key, peer, method, path, tsRaw, sigRaw)
}

// verifyPeerSig does the timestamp/sig work once the peer's key is resolved.
func verifyPeerSig(key ed25519.PublicKey, peer, method, path, tsRaw, sigRaw string) *PeerAuthError {
	if strings.TrimSpace(tsRaw) == "" || strings.TrimSpace(sigRaw) == "" {
		return &PeerAuthError{
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("federation peer %q announced an identity but is missing signature headers (%s, %s)", peer, FedAuthTsHeader, FedAuthSigHeader),
		}
	}
	var ts int64
	if _, err := fmt.Sscanf(tsRaw, "%d", &ts); err != nil {
		return &PeerAuthError{
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("%s must be a unix timestamp in seconds", FedAuthTsHeader),
		}
	}
	skew := time.Since(time.Unix(ts, 0))
	if skew > fedAuthSkew || skew < -fedAuthSkew {
		return &PeerAuthError{
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("%s outside allowed window (±%s)", FedAuthTsHeader, fedAuthSkew),
		}
	}
	sig, err := hex.DecodeString(strings.TrimSpace(sigRaw))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return &PeerAuthError{
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("%s is malformed: expected the hex encoding of a 64-byte ed25519 signature", FedAuthSigHeader),
		}
	}
	if !ed25519.Verify(key, FedAuthPayload(method, path, tsRaw), sig) {
		return &PeerAuthError{
			Status:  http.StatusUnauthorized,
			Message: fmt.Sprintf("federation peer %q signature verification failed", peer),
		}
	}
	return nil
}

// Middleware returns the federation peer-auth gate. A request WITHOUT the
// peer announcement header (PeerHeader) passes through untouched — local
// agent traffic and shared-secret-only deployments never see this gate. A
// request WITH the announcement must prove the announced identity: the
// gate verifies the signature BEFORE the handler (and the CR-CHAT-023
// policy layer) acts, so the peer claim the policy consults is a proven
// identity, not a string the caller chose.
func (a *PeerAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := strings.TrimSpace(r.Header.Get(PeerHeader))
		if peer == "" {
			next.ServeHTTP(w, r)
			return
		}
		if perr := a.VerifyPeerSignature(peer, r.Method, r.URL.Path,
			r.Header.Get(FedAuthTsHeader), r.Header.Get(FedAuthSigHeader)); perr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(perr.Status)
			_, _ = fmt.Fprintf(w, `{"error":%q}`, perr.Message)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SignFedRequest stamps the outbound federation peer-auth headers on req:
// X-Crier-Fed-Peer (the identity announcement, already set by the caller),
// X-Fed-Ts and X-Fed-Sig over FedAuthPayload. It is the outbound twin of
// VerifyPeerSignature — the destination proves this relay's identity the
// same way (mutual auth: both sides hold keys and verify each other).
func SignFedRequest(req *http.Request, key ed25519.PrivateKey, now time.Time) {
	ts := fmt.Sprintf("%d", now.Unix())
	payload := FedAuthPayload(req.Method, req.URL.Path, ts)
	sig := ed25519.Sign(key, payload)
	req.Header.Set(FedAuthTsHeader, ts)
	req.Header.Set(FedAuthSigHeader, hex.EncodeToString(sig))
}
