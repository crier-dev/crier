package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/crier-dev/crier/internal/trust"
	"github.com/jackc/pgx/v5/pgconn"
)

// TrustStore is an additive Store capability. Remote proxies without it fail
// explicitly rather than pretending to persist verifier-local trust state.
type TrustStore interface {
	AddTrustAnchor(trust.ChainAnchor) error
	ListTrustAnchors() ([]trust.ChainAnchor, error)
	AddTrustDelegation(trust.ChainDelegation) error
	ListTrustDelegations(subject string) ([]trust.ChainDelegation, error)
}

var ErrTrustExists = errors.New("trust record already exists (immutable)")
var ErrTrustUnsupported = errors.New("registry backend does not support trust records")

func (s *MemoryStore) AddTrustAnchor(a trust.ChainAnchor) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStoreInput, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, held := range s.trustAnchors {
		if held.ID == a.ID || strings.EqualFold(held.PublicKey, a.PublicKey) {
			return ErrTrustExists
		}
	}
	s.trustAnchors = append(s.trustAnchors, a)
	return nil
}
func (s *MemoryStore) ListTrustAnchors() ([]trust.ChainAnchor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]trust.ChainAnchor{}, s.trustAnchors...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *MemoryStore) AddTrustDelegation(d trust.ChainDelegation) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStoreInput, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkTrustDelegation(s.trustAnchors, s.trustDelegations, d); err != nil {
		return err
	}
	for _, held := range s.trustDelegations {
		if strings.EqualFold(held.Signature, d.Signature) {
			return ErrTrustExists
		}
	}
	d.Scope = append([]string{}, d.Scope...)
	s.trustDelegations = append(s.trustDelegations, d)
	return nil
}
func (s *MemoryStore) ListTrustDelegations(subject string) ([]trust.ChainDelegation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []trust.ChainDelegation{}
	for _, d := range s.trustDelegations {
		if subject == "" || strings.EqualFold(d.SubjectKey, subject) {
			d.Scope = append([]string{}, d.Scope...)
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signature < out[j].Signature })
	return out, nil
}

// Import requires a locally known voucher and a real signature, not a self
// asserted key in the HTTP body. Chain scope/depth are evaluated per use by
// VerifyChain: storage is evidence, not itself a grant.
func checkTrustDelegation(anchors []trust.ChainAnchor, delegations []trust.ChainDelegation, d trust.ChainDelegation) error {
	var key string
	for _, a := range anchors {
		if a.ID == d.Voucher {
			key = a.PublicKey
			break
		}
	}
	if key == "" {
		for _, held := range delegations {
			if trust.ChainKeyID(held.SubjectKey) == d.Voucher {
				key = held.SubjectKey
				break
			}
		}
	}
	if key == "" {
		return fmt.Errorf("%w: voucher is not locally held", ErrInvalidStoreInput)
	}
	pub, err := hex.DecodeString(key)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: invalid voucher key", ErrInvalidStoreInput)
	}
	sig, _ := hex.DecodeString(d.Signature)
	if !ed25519.Verify(pub, d.CanonicalBytes(), sig) {
		return fmt.Errorf("%w: TRUST_BAD_SIGNATURE for voucher %s", ErrInvalidStoreInput, d.Voucher)
	}
	return nil
}

func trustInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrTrustExists
	}
	return err
}
func (s *PostgresStore) AddTrustAnchor(a trust.ChainAnchor) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStoreInput, err)
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	_, err := s.pool.Exec(ctx, `INSERT INTO trust_anchors (id, public_key, label, created_at) VALUES ($1,$2,$3,$4)`, a.ID, strings.ToLower(a.PublicKey), a.Label, a.CreatedAt)
	return trustInsertError(err)
}
func (s *PostgresStore) ListTrustAnchors() ([]trust.ChainAnchor, error) {
	ctx, cancel := s.operationContext()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT id, public_key, label, created_at FROM trust_anchors ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []trust.ChainAnchor{}
	for rows.Next() {
		var a trust.ChainAnchor
		if err := rows.Scan(&a.ID, &a.PublicKey, &a.Label, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *PostgresStore) AddTrustDelegation(d trust.ChainDelegation) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStoreInput, err)
	}
	anchors, err := s.ListTrustAnchors()
	if err != nil {
		return err
	}
	held, err := s.ListTrustDelegations("")
	if err != nil {
		return err
	}
	if err := checkTrustDelegation(anchors, held, d); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	// Preserve exact signed JSON fields (including timestamp nanoseconds) in
	// JSONB. Scanning issued_at through timestamptz would change signed bytes.
	_, err = s.pool.Exec(ctx, `INSERT INTO trust_delegations (signature, subject_key, record) VALUES ($1,$2,$3::jsonb)`, strings.ToLower(d.Signature), strings.ToLower(d.SubjectKey), string(raw))
	return trustInsertError(err)
}
func (s *PostgresStore) ListTrustDelegations(subject string) ([]trust.ChainDelegation, error) {
	ctx, cancel := s.operationContext()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT record FROM trust_delegations WHERE ($1 = '' OR subject_key = $1) ORDER BY signature`, strings.ToLower(subject))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []trust.ChainDelegation{}
	for rows.Next() {
		var raw []byte
		var d trust.ChainDelegation
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *AuthorityStore) trustBackend() (TrustStore, error) {
	v, ok := s.Store.(TrustStore)
	if !ok {
		return nil, ErrTrustUnsupported
	}
	return v, nil
}
func (s *AuthorityStore) AddTrustAnchor(a trust.ChainAnchor) error {
	v, e := s.trustBackend()
	if e != nil {
		return e
	}
	return v.AddTrustAnchor(a)
}
func (s *AuthorityStore) ListTrustAnchors() ([]trust.ChainAnchor, error) {
	v, e := s.trustBackend()
	if e != nil {
		return nil, e
	}
	return v.ListTrustAnchors()
}
func (s *AuthorityStore) AddTrustDelegation(d trust.ChainDelegation) error {
	v, e := s.trustBackend()
	if e != nil {
		return e
	}
	return v.AddTrustDelegation(d)
}
func (s *AuthorityStore) ListTrustDelegations(subject string) ([]trust.ChainDelegation, error) {
	v, e := s.trustBackend()
	if e != nil {
		return nil, e
	}
	return v.ListTrustDelegations(subject)
}

var _ TrustStore = (*MemoryStore)(nil)
var _ TrustStore = (*PostgresStore)(nil)
var _ TrustStore = (*AuthorityStore)(nil)
