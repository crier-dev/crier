package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// EnsureResult reports what EnsureRegistered found or did. It is a report,
// not an error: an existing identity with a different public key is returned
// with KeyMatches=false so the caller can log the actionable remedy (the
// store cannot repair a stale registration without deleting data).
type EnsureResult struct {
	// Created is true when this call registered the agent.
	Created bool
	// Existed is true when the agent was already registered on the server.
	Existed bool
	// KeyMatches reports whether the server's registered public key equals
	// the local one. It is only meaningful when the local key is a full
	// ed25519 public key; otherwise it is false.
	KeyMatches bool
	// RegisteredPublicKey is the public key the server holds for the agent
	// after this call (the local key when Created).
	RegisteredPublicKey ed25519.PublicKey
}

// EnsureRegistered makes id exist on the remote server with the given
// ed25519 public key, idempotently.
//
// Behaviour:
//
//	GET /agents/{id} 200  -> already registered; report Existed=true and set
//	                         KeyMatches by comparing the returned public key
//	                         (hex, case-insensitive) with pub. A mismatch is
//	                         NOT an error — the caller logs it loudly.
//	GET /agents/{id} 404  -> POST /agents {id, public_key, capabilities};
//	                         report Created=true.
//	POST 409              -> another client registered the id between the GET
//	                         and the POST: re-GET and report Existed=true,
//	                         Created=false.
//
// Any other transport or status error is returned as-is, so callers can tell
// "the server said no" apart from "the server is unreachable". No key
// material beyond the public key ever leaves this function.
//
// Routes: this helper uses only GET /agents/{id} and POST /agents — both are
// deliberately outside the per-agent signature gate (see handler.go), so a
// bridge can register itself before it has any working key server-side. The
// agent-owned routes (inbox retrieve/ack/stats, DELETE /agents/{id}) stay
// gated and are never touched here.
func (s *RemoteStore) EnsureRegistered(id string, pub ed25519.PublicKey, capabilities []string) (EnsureResult, error) {
	if id == "" {
		return EnsureResult{}, fmt.Errorf("%w: agent id is required", ErrInvalidStoreInput)
	}
	if len(pub) != ed25519.PublicKeySize {
		return EnsureResult{}, fmt.Errorf("%w: public key must be %d bytes (ed25519), got %d",
			ErrInvalidStoreInput, ed25519.PublicKeySize, len(pub))
	}

	existing, err := s.Get(id)
	if err == nil {
		return identityResult(existing, pub), nil
	}
	if !errors.Is(err, ErrAgentNotFound) {
		return EnsureResult{}, err
	}

	// PublicKey is a HexKey ([]byte with hex MarshalJSON); pass it as the
	// typed value so it serializes as a hex string, matching POST /agents.
	regErr := s.Register(&Agent{ID: id, PublicKey: HexKey(pub), Capabilities: capabilities})
	if regErr == nil {
		return EnsureResult{Created: true, KeyMatches: true, RegisteredPublicKey: pub}, nil
	}
	if !errors.Is(regErr, ErrAgentExists) {
		return EnsureResult{}, regErr
	}

	// Lost the race: someone else registered the id after our GET. Re-read
	// and report what is actually there rather than assuming our key won.
	raced, getErr := s.Get(id)
	if getErr != nil {
		return EnsureResult{}, getErr
	}
	return identityResult(raced, pub), nil
}

// identityResult builds the result for an agent that already exists.
func identityResult(existing *Agent, pub ed25519.PublicKey) EnsureResult {
	registered := ed25519.PublicKey(existing.PublicKey)
	return EnsureResult{
		Existed:             true,
		KeyMatches:          publicKeysMatch(registered, pub),
		RegisteredPublicKey: registered,
	}
}

// publicKeysMatch compares two ed25519 public keys case-insensitively on
// their hex form. A key that is not exactly ed25519.PublicKeySize bytes
// cannot be compared and never matches.
func publicKeysMatch(a, b ed25519.PublicKey) bool {
	if len(a) != ed25519.PublicKeySize || len(b) != ed25519.PublicKeySize {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(a), hex.EncodeToString(b))
}
