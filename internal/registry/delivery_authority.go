package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrServiceAuthorityRequired is returned by every non-HTTP inbox writer when
// it attempts to write to a protected service recipient.  The authority value
// is intentionally opaque outside this package: untrusted envelope fields can
// never manufacture it.
var ErrServiceAuthorityRequired = errors.New("service delivery authority required")

var ErrServiceIdempotencyConflict = errors.New("service delivery idempotency key is bound to a different body")

// ServiceDeliveryReceipt is the durable HTTP accept associated with a
// protected delivery. Response is the complete JSON response body.
type ServiceDeliveryReceipt struct {
	Status     int
	MessageID  string
	BodyDigest string
	Response   []byte
}

// ServiceIdempotentStore is deliberately separate from Store's legacy
// Deliver operation. Protected delivery needs a durable, body-bound receipt;
// ordinary delivery must retain its existing idempotency behavior.
type ServiceIdempotentStore interface {
	LookupServiceDelivery(sender, recipient, key, bodyDigest string) (*ServiceDeliveryReceipt, error)
	DeliverServiceIdempotent(sender, recipient, key, bodyDigest string, entry *InboxEntry, receipt ServiceDeliveryReceipt) (*ServiceDeliveryReceipt, bool, error)
}

// ServiceAuthority is minted only after the signed HTTP contract has been
// verified.  Its fields are private so body sender/principal/reply metadata is
// never itself treated as authorization.
type ServiceAuthority struct {
	sender, recipient, idempotencyKey, bodyDigest, audience string
	rawBody                                                 []byte
}

// AuthorityStore is the single policy choke point shared by HTTP, sessions,
// Dagger and in-process/direct-database MCP writers.
type AuthorityStore struct {
	Store
	policy   ServiceDeliveryPolicy
	audience string
}

func NewAuthorityStore(store Store, policy ServiceDeliveryPolicy, audience string) *AuthorityStore {
	if policy == nil {
		policy = ServiceDeliveryPolicy{}
	}
	return &AuthorityStore{Store: store, policy: policy, audience: audience}
}

func (s *AuthorityStore) Deliver(agentID string, entry *InboxEntry) error {
	if _, protected := s.policy[agentID]; protected {
		return fmt.Errorf("%w for recipient %q", ErrServiceAuthorityRequired, agentID)
	}
	return s.Store.Deliver(agentID, entry)
}

func (s *AuthorityStore) lookupAuthorized(a *ServiceAuthority) (*ServiceDeliveryReceipt, error) {
	store, ok := s.Store.(ServiceIdempotentStore)
	if !ok {
		return nil, fmt.Errorf("protected delivery backend lacks durable idempotency")
	}
	return store.LookupServiceDelivery(a.sender, a.recipient, a.idempotencyKey, a.bodyDigest)
}

func (s *AuthorityStore) deliverServiceAuthorized(agentID string, entry *InboxEntry, a *ServiceAuthority, receipt ServiceDeliveryReceipt) (*ServiceDeliveryReceipt, bool, error) {
	if err := s.validateAuthority(agentID, entry, a); err != nil {
		return nil, false, err
	}
	store, ok := s.Store.(ServiceIdempotentStore)
	if !ok {
		return nil, false, fmt.Errorf("protected delivery backend lacks durable idempotency")
	}
	return store.DeliverServiceIdempotent(a.sender, agentID, a.idempotencyKey, a.bodyDigest, entry, receipt)
}

func (s *AuthorityStore) validateAuthority(agentID string, entry *InboxEntry, a *ServiceAuthority) error {
	allowed, protected := s.policy[agentID]
	if !protected {
		return nil
	}
	if a == nil || a.recipient != agentID || a.sender != entry.Sender || a.idempotencyKey == "" || a.idempotencyKey != entry.IdempotencyKey || (s.audience != "" && a.audience != s.audience) {
		return fmt.Errorf("%w for recipient %q", ErrServiceAuthorityRequired, agentID)
	}
	if _, ok := allowed[a.sender]; !ok {
		return fmt.Errorf("%w for sender %q recipient %q", ErrServiceAuthorityRequired, a.sender, agentID)
	}
	digest := sha256.Sum256(a.rawBody)
	if a.bodyDigest == "" || a.bodyDigest != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("%w: request body digest mismatch", ErrServiceAuthorityRequired)
	}
	return nil
}

func (s *AuthorityStore) Policy() ServiceDeliveryPolicy { return s.policy }
func (s *AuthorityStore) Audience() string              { return s.audience }

func (s *AuthorityStore) ListError() error {
	if reporter, ok := s.Store.(ListErrorReporter); ok {
		return reporter.ListError()
	}
	return nil
}
func (s *AuthorityStore) Update(agent *Agent) error {
	if store, ok := s.Store.(updater); ok {
		return store.Update(agent)
	}
	return errors.New("registry store does not support agent updates")
}
func (s *AuthorityStore) PurgeExpiredReport(report func(agentID string, entry *InboxEntry)) int {
	if store, ok := s.Store.(PurgeReporter); ok {
		return store.PurgeExpiredReport(report)
	}
	return s.PurgeExpired()
}
func (s *AuthorityStore) AppendDeadLetter(dl *DeadLetter) (bool, error) {
	if store, ok := s.Store.(DeadLetterStore); ok {
		return store.AppendDeadLetter(dl)
	}
	return false, errors.New("registry store does not keep a dead-letter destination")
}
func (s *AuthorityStore) ListDeadLetters(agentID string, limit int) ([]*DeadLetter, error) {
	if store, ok := s.Store.(DeadLetterStore); ok {
		return store.ListDeadLetters(agentID, limit)
	}
	return nil, errors.New("registry store does not keep a dead-letter destination")
}
func (s *AuthorityStore) Healthy() bool                       { return s.Store.(HealthReporter).Healthy() }
func (s *AuthorityStore) Touch(id string, at time.Time) error { return s.Store.(Toucher).Touch(id, at) }
func (s *AuthorityStore) QueueDepth() (QueueDepth, error) {
	return s.Store.(DepthReporter).QueueDepth()
}
func (s *AuthorityStore) Peek(id, mid string) (*InboxEntry, error) {
	return s.Store.(InboxPeeker).Peek(id, mid)
}
func (s *AuthorityStore) PeekInbox(id string) ([]*InboxEntry, error) {
	return s.Store.(InboxLister).PeekInbox(id)
}
func (s *AuthorityStore) CloseEntry(id, mid string) error {
	return s.Store.(InboxCloser).CloseEntry(id, mid)
}
func (s *AuthorityStore) RevokeLeases(id string) (int, error) {
	return s.Store.(LeaseRevoker).RevokeLeases(id)
}
func (s *AuthorityStore) Transfer(id, lease string, mids []string, target string, force bool) (int, error) {
	return s.Store.(Transferrer).Transfer(id, lease, mids, target, force)
}
func (s *AuthorityStore) Close() {
	if c, ok := s.Store.(interface{ Close() }); ok {
		c.Close()
	}
}
