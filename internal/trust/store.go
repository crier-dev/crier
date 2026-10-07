package trust

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Store is a verifier's LOCAL trust state (spec §4.4): its anchors, the
// delegation records it is willing to hold, its rotation certificates and its
// revocation tombstones. It is thread-safe and resolved PER CHECK — revocation
// and expiry take effect on the next verification, not on a cached verdict
// (§7.4). A peer cannot inject an anchor here; every AddAnchor is the local
// operator's decision. All collections are append-only (tombstones are never
// edited away, §7.4); Revoke keeps the operative record per key in a map for
// O(1) checks while callers wanting an audit trail persist the records they
// pass in.
type Store struct {
	mu          sync.RWMutex
	anchors     []Anchor
	delegations []Delegation
	rotations   []RotationCert
	revocations map[string]Revocation
	keyBytes    map[string]ed25519.PublicKey
}

// NewStore returns an empty verifier state.
func NewStore() *Store {
	return &Store{
		revocations: map[string]Revocation{},
		keyBytes:    map[string]ed25519.PublicKey{},
	}
}

// AddAnchor records a locally-accepted trust anchor (§3.1). The anchor's
// key_id is derived from the public key, not taken on faith: a caller that
// passes a mismatched key_id gets an error, because an anchor whose name and
// bytes disagree would poison every walk that terminates at it.
func (s *Store) AddAnchor(a Anchor) error {
	pub, err := decodeKey(a.PublicKey)
	if err != nil {
		return fmt.Errorf("trust: anchor %s: %w", a.ID, err)
	}
	derived := keyID(pub)
	if a.KeyID == "" {
		a.KeyID = derived
	} else if a.KeyID != derived {
		return fmt.Errorf("trust: anchor key_id %q does not match its public key (%s)", a.KeyID, derived)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchors = append(s.anchors, a)
	s.keyBytes[a.KeyID] = pub
	return nil
}

// AddDelegation imports a delegation record after verifying the issuer's
// signature over the canonical bytes (§4.1). Unverified records are never
// held: publishing is not believing (§4.4). The issuer's key must already be
// resolvable — an anchor or a previously imported delegation subject.
func (s *Store) AddDelegation(d Delegation) error {
	pub, err := decodeKey(d.PublicKey)
	if err != nil {
		return fmt.Errorf("trust: delegation %s subject key: %w", d.ID, err)
	}
	derived := keyID(pub)
	if d.SubjectKey == "" {
		d.SubjectKey = derived
	} else if d.SubjectKey != derived {
		return fmt.Errorf("trust: delegation subject_key_id %q does not match its public key (%s)", d.SubjectKey, derived)
	}
	issuerPub, ok := s.publicKey(d.IssuerKey)
	if !ok {
		return fmt.Errorf("trust: delegation issuer key %s is not held; import it first (publishing is not believing, §4.4)", d.IssuerKey)
	}
	sig, err := hex.DecodeString(d.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("trust: delegation %s signature is malformed: %w", d.ID, ErrMalformed)
	}
	if !ed25519.Verify(issuerPub, d.canonical(), sig) {
		return fmt.Errorf("trust: delegation signature by %s does not verify: %w", d.IssuerKey, ErrSignatureInvalid)
	}
	// §7.3: revocation ends the key's FORWARD authority — a revoked key can
	// never sign a new delegation. History is untouched; the walk still
	// annotates pre-revocation records revoked-after-signing.
	if r, ok := s.revokedAt(d.IssuerKey); ok && !d.NotBefore.Before(r.RevokedAt) {
		return fmt.Errorf("trust: issuer %s was revoked at %s — a revoked key cannot vouch onward: %w",
			d.IssuerKey, r.RevokedAt.Format(time.RFC3339), ErrKeyRevoked)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delegations = append(s.delegations, d)
	s.keyBytes[d.SubjectKey] = pub
	return nil
}

// AddRotation imports a rotation certificate after verifying it is signed by
// the OUTGOING key (§6.1). A certificate signed by anything else is refused
// with the named rotation error — the incoming key cannot vouch for itself
// (TRUST_ROTATION_UNSIGNED). NewPublicKey carries the incoming key's bytes for
// the verifier's own resolution; it is not part of the signed transcript.
func (s *Store) AddRotation(c RotationCert) error {
	oldPub, ok := s.publicKey(c.OldKeyID)
	if !ok {
		return fmt.Errorf("trust: rotation old key %s is not held", c.OldKeyID)
	}
	sig, err := hex.DecodeString(c.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("trust: rotation signature is malformed: %w", ErrMalformed)
	}
	if !ed25519.Verify(oldPub, c.rotationTranscript(), sig) {
		return fmt.Errorf("trust: rotation certificate not signed by the outgoing key %s: %w", c.OldKeyID, ErrRotationUnsigned)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotations = append(s.rotations, c)
	if pub, err := decodeKey(c.NewPublicKey); err == nil && keyID(pub) == c.NewKeyID {
		s.keyBytes[c.NewKeyID] = pub
	}
	return nil
}

// Revoke appends a revocation tombstone (§7.4). It is never removed; the map
// keeps the operative record per key. Revocation takes effect on the NEXT
// verification (§7.4), never on a cached verdict.
func (s *Store) Revoke(r Revocation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revocations[r.KeyID] = r
}

// RegisterKey makes an out-of-band public key resolvable by key_id — the
// minimal way a key enters the known-key set (§5.1 step 1) without pretending
// to be an anchor. It grants NOTHING by itself: §3.2 still applies, and a
// registered-but-unvouched key is refused with ErrNoPathToAnchor.
func (s *Store) RegisterKey(pub ed25519.PublicKey) string {
	id := keyID(pub)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyBytes[id] = pub
	return id
}

// revokedAt returns the operative tombstone for a key, if any.
func (s *Store) revokedAt(keyID string) (Revocation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.revocations[keyID]
	return r, ok
}

// anchorsHeld returns a copy of the anchor list.
func (s *Store) anchorsHeld() []Anchor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Anchor, len(s.anchors))
	copy(out, s.anchors)
	return out
}

// delegationsFor returns held delegations whose SUBJECT is keyID.
func (s *Store) delegationsFor(keyID string) []Delegation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Delegation
	for _, d := range s.delegations {
		if d.SubjectKey == keyID {
			out = append(out, d)
		}
	}
	return out
}

// publicKey resolves a held key id to its bytes: anchors and delegation
// subjects carry their keys; rotation imports and RegisterKey fill the rest.
func (s *Store) publicKey(keyID string) (ed25519.PublicKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pub, ok := s.keyBytes[keyID]
	return pub, ok
}

func decodeKey(hexKey string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is not 32-byte hex ed25519: %w", ErrMalformed)
	}
	return ed25519.PublicKey(raw), nil
}
