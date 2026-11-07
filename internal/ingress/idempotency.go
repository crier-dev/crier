package ingress

// idempotency.go — the ingress key ledger (CRIER-INGRESS §3).
//
// One row per (namespace, key): the hash of the request that first used the key
// and the EXACT result body (and status) that request received. A replay with
// the same key and the same hash is answered from the stored row — same body,
// same status — so a caller that lost the connection can ask again instead of
// duplicating a session, a member join or an inbox entry. A replay with the
// same key and a DIFFERENT hash is 409 INGRESS_KEY_REUSED: the key already means
// another message, and silently reusing it would make two different requests
// indistinguishable to the receiver.
//
// Scope, stated rather than implied: the wired implementation is
// process-scoped and in-memory, the same scope the registry's own sender-key
// window has (internal/registry/idempotency.go). The INTERFACE is the seam the
// spec's durable table (ingress_idempotency / .coding-hermes/
// ingress-idempotency.jsonl) plugs into; this row does not add a fourth
// session-backend schema to reach it. Inside one process the guarantee is still
// exactly-once, because the caller holds session.Repository.LockMessage around
// the lookup, the write and the save (§3, §6 "new_session race").

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/session"
)

// IdempotencyRecord is one key's stored answer.
type IdempotencyRecord struct {
	// RequestHash is the canonical hash of the request that claimed the key.
	RequestHash string
	// Result is the response body exactly as it was written, so a replay is
	// byte-identical to the original answer.
	Result json.RawMessage
	// Status is the status the original answer carried (200 for an existing
	// session, 201 for a created one).
	Status int
	// CreatedAt is when the key was first claimed.
	CreatedAt time.Time
}

// IdempotencyStore records (namespace, key) -> the first request's hash and
// answer. Implementations must be safe for concurrent use: two requests can
// arrive on the same key, and the loser of the repository lock must still read
// the winner's row after it releases it.
type IdempotencyStore interface {
	// Lookup returns the stored record and whether one exists.
	Lookup(ctx context.Context, namespace, key string) (IdempotencyRecord, bool, error)
	// Save records the first answer for a key. Saving an existing key again is
	// a no-op that leaves the FIRST answer in place: the winner's row is the
	// replay authority, and a later writer never overwrites it.
	Save(ctx context.Context, namespace, key string, rec IdempotencyRecord) error
}

// MemoryIdempotencyStore is the process-scoped ledger.
type MemoryIdempotencyStore struct {
	mu   sync.Mutex
	rows map[string]IdempotencyRecord
}

// NewMemoryIdempotencyStore returns an empty ledger.
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{rows: make(map[string]IdempotencyRecord)}
}

func (s *MemoryIdempotencyStore) key(namespace, key string) string {
	return namespace + "\x00" + key
}

// Lookup implements IdempotencyStore.
func (s *MemoryIdempotencyStore) Lookup(_ context.Context, namespace, key string) (IdempotencyRecord, bool, error) {
	if s == nil {
		return IdempotencyRecord{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.rows[s.key(namespace, key)]
	if !ok {
		return IdempotencyRecord{}, false, nil
	}
	// Copy the body: the caller may hand it straight to a ResponseWriter, and
	// a shared slice must not be mutable through the map's stored value.
	out := rec
	out.Result = append(json.RawMessage(nil), rec.Result...)
	return out, true, nil
}

// Save implements IdempotencyStore.
func (s *MemoryIdempotencyStore) Save(_ context.Context, namespace, key string, rec IdempotencyRecord) error {
	if s == nil {
		return fmt.Errorf("ingress idempotency: no store wired")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(namespace, key)
	if _, exists := s.rows[k]; exists {
		return nil
	}
	stored := rec
	stored.Result = append(json.RawMessage(nil), rec.Result...)
	s.rows[k] = stored
	return nil
}

// requestHash is the canonical fingerprint of the DESTINATION-AFFECTING part of
// a request. The idempotency key itself is excluded (the key is the lookup, not
// part of the message), and so is anything cosmetic: the hash is over the
// parsed, normalized Request plus the realm it acts in, so two spellings of the
// same push ("tags":["a"] vs "tags":["a","a"]) hash alike while a changed text,
// target, author, tag, shared payload or reply target does not.
func requestHash(req *Request, realm string) (string, error) {
	shape := struct {
		Realm      string              `json:"realm"`
		Mode       Mode                `json:"mode"`
		SessionID  string              `json:"session_id,omitempty"`
		Template   *SessionTemplate    `json:"template,omitempty"`
		AgentID    string              `json:"agent_id,omitempty"`
		Capability string              `json:"capability,omitempty"`
		Harness    string              `json:"harness,omitempty"`
		Author     session.AuthorRef   `json:"author"`
		Kind       session.MessageKind `json:"kind"`
		Text       string              `json:"text"`
		Tags       []string            `json:"tags,omitempty"`
		Payload    json.RawMessage     `json:"payload,omitempty"`
		ReplyTo    string              `json:"reply_to_thread,omitempty"`
	}{
		Realm: realm, Mode: req.Mode, SessionID: req.SessionID, Template: req.Template,
		AgentID: req.AgentID, Capability: req.Capability, Harness: req.Harness,
		Author: req.Author, Kind: req.Kind,
		Text: req.Text, Tags: req.Tags, Payload: req.Payload, ReplyTo: req.ReplyTo,
	}
	canonical, err := session.CanonicalJSON(shape)
	if err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
