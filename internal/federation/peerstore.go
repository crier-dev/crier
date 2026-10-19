package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Verdict is the identity/link decision, distinct from a transport outcome.
type Verdict string

const (
	FedAllowed      Verdict = "FED_ALLOWED"
	FedNoLink       Verdict = "FED_NO_LINK"
	FedPeerRevoked  Verdict = "FED_PEER_REVOKED"
	FedBadSignature Verdict = "FED_BAD_SIGNATURE"
	FedTokenOnly    Verdict = "FED_TOKEN_ONLY"
	VerdictHeader           = "X-Crier-Fed-Verdict"
)

// PeerRecord is an instance identity, not an agent or a policy. PublicKey and
// URL are pinned by the operator; PATCH cannot replace either of them.
type PeerRecord struct {
	ID        string     `json:"id"`
	PublicKey string     `json:"public_key"`
	KeyID     string     `json:"key_id"`
	URL       string     `json:"url"`
	Direction string     `json:"direction"`
	State     string     `json:"state"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	LinkedAt  *time.Time `json:"linked_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// KeyID names the instance key using the detect signer's SHA256 prefix convention.
func KeyID(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:8])
}

type peerDocument struct {
	Version int          `json:"version"`
	Peers   []PeerRecord `json:"peers"`
}

// PeerStore is a single-process, durable peer registry. Mutations publish in
// memory only after write+fsync+atomic rename, in the hold file's directory.
// A corrupt existing file is a startup error: recovering an older snapshot
// could resurrect a revoked credential, so auth state never rolls back to .bak.
type PeerStore struct {
	mu          sync.RWMutex
	path        string
	peers       map[string]PeerRecord
	requirePeer bool
}

func OpenPeerStore(path string) (*PeerStore, error) {
	if path == "" {
		return nil, errors.New("federation: peer store path is empty")
	}
	s := &PeerStore{path: path, peers: make(map[string]PeerRecord)}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var doc peerDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("federation: parse peer store: %w", err)
	}
	if doc.Version != 1 {
		return nil, errors.New("federation: unsupported peer store version")
	}
	for _, p := range doc.Peers {
		if err := validatePeer(p); err != nil {
			return nil, err
		}
		if _, found := s.peers[p.ID]; found {
			return nil, errors.New("federation: duplicate peer id")
		}
		for _, old := range s.peers {
			if old.URL == p.URL {
				return nil, errors.New("federation: duplicate peer url")
			}
		}
		s.peers[p.ID] = p
	}
	return s, nil
}

func validatePeer(p PeerRecord) error {
	if p.ID == "" || strings.TrimSpace(p.ID) != p.ID || strings.ContainsAny(p.ID, "/\r\n\t") {
		return errors.New("federation: invalid instance id")
	}
	key, err := parseEd25519PublicKey(p.PublicKey)
	if err != nil {
		return err
	}
	if p.KeyID != KeyID(key) {
		return errors.New("federation: key_id does not match public_key")
	}
	if p.Direction != "inbound" && p.Direction != "outbound" && p.Direction != "both" {
		return errors.New("federation: direction must be inbound, outbound or both")
	}
	if p.State != "pending" && p.State != "linked" && p.State != "revoked" {
		return errors.New("federation: state must be pending, linked or revoked")
	}
	link, ok := parseLink(p.URL)
	if !ok || link.URL != p.URL {
		return errors.New("federation: url must be a canonical http(s) base URL")
	}
	return nil
}

// SetRequirePeer refuses keyless inbox ingress while any lifecycle records
// exist. Empty stores retain the legacy deployment-token path.
func (s *PeerStore) SetRequirePeer(required bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requirePeer = required
}

// InboundPosture is a secret-free snapshot of the keyless inbox requirement.
// Peers counts ALL lifecycle records, including pending and revoked records:
// these arm the existing refusal just as linked records do.
// TokenOnlyAccepted describes this gate, not downstream deployment authorization.
type InboundPosture struct {
	RequirePeerConfigured bool
	RequirePeerEffective  bool
	Peers                 int
	TokenOnlyAccepted     bool
}

// InboundPosture derives reporting and enforcement from one locked snapshot.
// A nil store has no refusal gate, even if the deployment requested one.
func (s *PeerStore) InboundPosture() InboundPosture {
	if s == nil {
		return InboundPosture{TokenOnlyAccepted: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	effective := s.requirePeer && len(s.peers) > 0
	return InboundPosture{
		RequirePeerConfigured: s.requirePeer,
		RequirePeerEffective:  effective,
		Peers:                 len(s.peers),
		TokenOnlyAccepted:     !effective,
	}
}

func (s *PeerStore) List() []PeerRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return peerList(s.peers)
}

func peerList(peers map[string]PeerRecord) []PeerRecord {
	out := make([]PeerRecord, 0, len(peers))
	for _, p := range peers {
		out = append(out, clonePeer(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func clonePeer(p PeerRecord) PeerRecord {
	if p.LinkedAt != nil {
		v := *p.LinkedAt
		p.LinkedAt = &v
	}
	if p.RevokedAt != nil {
		v := *p.RevokedAt
		p.RevokedAt = &v
	}
	return p
}

func (s *PeerStore) Get(id string) (PeerRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.peers[id]
	return clonePeer(p), ok
}

func (s *PeerStore) ForURL(url string) (PeerRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.peers {
		if p.URL == url {
			return clonePeer(p), true
		}
	}
	return PeerRecord{}, false
}

// Create pins a key in pending state. Only a successful handshake links it.
func (s *PeerStore) Create(p PeerRecord) (PeerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.peers[p.ID]; exists {
		return PeerRecord{}, errors.New("FED_PEER_EXISTS")
	}
	for _, old := range s.peers {
		if old.URL == p.URL {
			return PeerRecord{}, errors.New("FED_PEER_URL_EXISTS")
		}
	}
	key, err := parseEd25519PublicKey(p.PublicKey)
	if err != nil {
		return PeerRecord{}, err
	}
	p.PublicKey = hex.EncodeToString(key)
	p.KeyID = KeyID(key)
	p.State = "pending"
	p.LinkedAt = nil
	p.RevokedAt = nil
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	if err := validatePeer(p); err != nil {
		return PeerRecord{}, err
	}
	return p, s.putLocked(p)
}

type PeerPatch struct {
	State     *string `json:"state,omitempty"`
	Direction *string `json:"direction,omitempty"`
	Label     *string `json:"label,omitempty"`
}

func (s *PeerStore) Patch(id string, patch PeerPatch) (PeerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	if !ok {
		return PeerRecord{}, errors.New(string(FedNoLink))
	}
	if patch.State != nil {
		if *patch.State == "linked" && p.State != "linked" {
			return PeerRecord{}, errors.New("FED_HANDSHAKE_REQUIRED")
		}
		p.State = *patch.State
		if p.State == "revoked" {
			now := time.Now().UTC()
			p.RevokedAt = &now
		}
	}
	if patch.Direction != nil {
		p.Direction = *patch.Direction
	}
	if patch.Label != nil {
		p.Label = *patch.Label
	}
	p.UpdatedAt = time.Now().UTC()
	if err := validatePeer(p); err != nil {
		return PeerRecord{}, err
	}
	return clonePeer(p), s.putLocked(p)
}

// link rechecks the pinned key and revocation under the mutation lock.
func (s *PeerStore) link(id, keyID string) (PeerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	if !ok {
		return PeerRecord{}, errors.New(string(FedNoLink))
	}
	if p.State == "revoked" {
		return PeerRecord{}, errors.New(string(FedPeerRevoked))
	}
	if p.KeyID != keyID {
		return PeerRecord{}, errors.New(string(FedBadSignature))
	}
	now := time.Now().UTC()
	p.State = "linked"
	p.LinkedAt = &now
	p.UpdatedAt = now
	return clonePeer(p), s.putLocked(p)
}

func (s *PeerStore) putLocked(p PeerRecord) error {
	next := make(map[string]PeerRecord, len(s.peers)+1)
	for id, old := range s.peers {
		next[id] = old
	}
	next[p.ID] = p
	raw, err := json.Marshal(peerDocument{Version: 1, Peers: peerList(next)})
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".fed-peers-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	// Rename is the publication point. Keep memory consistent even if the
	// directory fsync reports a durability failure (returned, never hidden).
	s.peers = next
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func peerVerdict(p PeerRecord, found bool) Verdict {
	if !found {
		return FedNoLink
	}
	if p.State == "revoked" {
		return FedPeerRevoked
	}
	if p.State != "linked" {
		return FedNoLink
	}
	return FedAllowed
}
