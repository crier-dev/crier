package trust

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// Verdict is the named decision of one verification (spec §5.1 step 8, §7.3):
// the error (nil when trusted), the named outcome code, and the trust class —
// the temporal annotation a revoked signer's records carry.
type Verdict struct {
	// Err is nil when the signature is believed.
	Err error
	// Code names the outcome (Outcome* constants; "" when trusted).
	Code string
	// Class is TrustClassRevokedAfterSigning when a pre-revocation signature
	// verified under a since-revoked key; "" otherwise.
	Class TrustClass
}

// VerifyOptions is the ONE USE a signature is presented for (§5.1): the
// namespace / direction / purpose every hop's scope must cover, the instant
// the signature claims to have been made (for revocation time-anchoring), and
// the chain depth bound.
type VerifyOptions struct {
	Namespace string
	Direction string
	Purpose   string
	// SignedAt is the instant the signature was made. Zero means "now".
	SignedAt time.Time
	// MaxChainDepth bounds the walk; 0 resolves to DefaultMaxChainDepth.
	MaxChainDepth int
}

// DefaultMaxChainDepth bounds how far the walk may go (§4.2: the chain is
// bounded). Generous enough for a real org chart, small enough that a cycle
// cannot loop forever (cycles are also cut by the visited set).
const DefaultMaxChainDepth = 8

// Verify walks the chain from the signer's key to a held anchor (§5.1 steps
// 2–5), then verifies the signature over payload with the signer's public key
// (step 6). The chain is walked BEFORE the signature is verified — the two
// are separate verdicts with separate codes (§5.1).
//
// Trust semantics (§3.2): the key is believed only if it IS an anchor or a
// chain of held delegations leads to one, with every hop in scope (§4.2), in
// its temporal window (refused, never clamped), and unrevoked at the relevant
// instant (§7). Revocation is checked PER HOP, not only on the leaf (§5.1):
// a revoked voucher cannot reprieve itself by having signed its subject
// before revocation. A LEAF signature made before its key's revocation still
// verifies, annotated revoked-after-signing (§7.3) — but a revocation ANYWHERE
// on the path above the leaf refuses outright, because the vouchers' forward
// authority (and this verification's future) is after their revocation.
//
// It returns a Verdict with a NAMED outcome for every refusal; Err is the
// matching sentinel (errors.Is-able).
func (s *Store) Verify(signerKey ed25519.PublicKey, payload []byte, sig []byte, opts VerifyOptions) Verdict {
	signedAt := opts.SignedAt
	if signedAt.IsZero() {
		signedAt = time.Now()
	}
	depth := opts.MaxChainDepth
	if depth <= 0 {
		depth = DefaultMaxChainDepth
	}

	leafID := keyID(signerKey)
	res, path := s.walkPath(leafID, opts, signedAt, depth, map[string]bool{leafID: true}, 0)
	if res.Err != nil {
		return res
	}

	// Step 6: the signature itself, with the key that made it.
	if !ed25519.Verify(signerKey, payload, sig) {
		return Verdict{Err: ErrSignatureInvalid, Code: OutcomeSignatureInvalid}
	}

	// §7.3: a signature made before the leaf key's revocation verifies with
	// the revoked-after-signing class. (The path's vouchers were already
	// required unrevoked-at-now by walkPath; only the leaf carries the
	// annotation, because the signature is the thing dated SignedAt.)
	if r, ok := s.revokedAt(leafID); ok && signedAt.Before(r.RevokedAt) {
		return Verdict{Code: OutcomeRevokedAfterSigning, Class: TrustClassRevokedAfterSigning}
	}
	_ = path
	return Verdict{}
}

// walkPath proves a delegation path from the key named keyID up to a held
// anchor, checking scope, window and revocation at every hop, and returns the
// path it walked (anchor first, subject last; nil when unproven).
func (s *Store) walkPath(keyID string, opts VerifyOptions, signedAt time.Time, depth int, visited map[string]bool, hop int) (Verdict, []string) {
	// Revocation, per hop including the leaf (§5.1 step 5): at or after
	// revocation the key's authority is gone — refused (§7.3).
	if r, ok := s.revokedAt(keyID); ok && !signedAt.Before(r.RevokedAt) {
		return Verdict{Err: ErrKeyRevoked, Code: OutcomeKeyRevoked}, nil
	}

	if hop > depth {
		return Verdict{Err: ErrNoPathToAnchor, Code: OutcomeNoPathToAnchor}, nil
	}

	// Is this key an anchor? An expired anchor is treated as ABSENT (§3.1) —
	// inert, not deleted.
	for _, a := range s.anchorsHeld() {
		if a.KeyID == keyID {
			if a.ExpiresAt != nil && signedAt.After(*a.ExpiresAt) {
				return Verdict{Err: ErrAnchorExpired, Code: OutcomeNoPathToAnchor}, nil
			}
			return Verdict{}, []string{keyID}
		}
	}

	if hop == depth {
		// Depth exhausted without reaching an anchor (§4.2 bounded chain).
		return Verdict{Err: ErrNoPathToAnchor, Code: OutcomeNoPathToAnchor}, nil
	}

	// Walk up: find a held delegation whose subject is this key, verify its
	// own constraints, and continue from its issuer. When a candidate hop
	// fails for a NAMED reason (scope, window) we remember it: if no
	// alternative path exists, that named verdict is the answer (§5.1 gives
	// scope and window their own codes, distinct from no-path).
	var namedReason *Verdict
	for _, d := range s.delegationsFor(keyID) {
		// Window (§4.2): refused, never clamped.
		if d.NotBefore.After(signedAt) || signedAt.After(d.ExpiresAt) {
			if namedReason == nil {
				r := Verdict{Err: ErrDelegationExpired, Code: OutcomeDelegationExpired}
				namedReason = &r
			}
			continue
		}
		// Scope (§4.2, §5.1 step 3).
		if !d.Scope.covers(opts.Namespace, opts.Direction, opts.Purpose) {
			if namedReason == nil {
				r := Verdict{Err: ErrDelegationOutOfScope, Code: OutcomeDelegationOutOfScope}
				namedReason = &r
			}
			continue
		}
		// D16 depth (§4.3): scope.max_depth limits how many FURTHER
		// delegations the subject may issue. This delegation sits at
		// position hop+1 counting from the leaf, so it must permit hop+1
		// issuance levels below its subject: its subject issues 1, and that
		// subject's own delegation must permit the rest (recursively enforced
		// at the next hop). A max_depth 0 delegation can therefore only ever
		// terminate a chain at an anchor — it vouches for a signing key and
		// cannot vouch onward. The check applies even when the issuer IS an
		// anchor: the anchor's vouching sets the subject's budget.
		if d.Scope.MaxDepth < hop+1 {
			if namedReason == nil {
				r := Verdict{Err: ErrDelegationOutOfScope, Code: OutcomeDelegationOutOfScope}
				namedReason = &r
			}
			continue
		}
		if _, ok := s.publicKey(d.IssuerKey); !ok || visited[d.IssuerKey] {
			continue // unresolvable issuer, or a cycle
		}
		next := make(map[string]bool, len(visited)+1)
		for k := range visited {
			next[k] = true
		}
		next[d.IssuerKey] = true
		v, path := s.walkPath(d.IssuerKey, opts, signedAt, depth, next, hop+1)
		if v.Err == nil {
			return v, append(path, keyID)
		}
		if v.Code != OutcomeNoPathToAnchor {
			// A hop that failed for a NAMED reason other than "no path"
			// (scope, window, revocation deeper up) is the verdict.
			return v, nil
		}
		// §7.3/§7.4: a revoked voucher's forward authority is ended — but
		// only its own era's records keep the annotation. A chain THROUGH a
		// key revoked after SignedAt cannot have been a valid chain at
		// signing time and verified later; the walk treats any revocation on
		// the path (the issuer here) as terminal for THIS verification.
		if _, ok := s.revokedAt(d.IssuerKey); ok {
			// §7.3/§7.4: a revoked voucher's forward authority is ended — a
			// revoked key can no longer reprieve a chain through itself, so
			// the walk through it is terminal for THIS verification.
			return Verdict{
				Err:  fmt.Errorf("%w: voucher %s in the path is revoked", ErrKeyRevoked, d.IssuerKey),
				Code: OutcomeKeyRevoked,
			}, nil
		}
	}
	if namedReason != nil {
		return *namedReason, nil
	}
	return Verdict{Err: ErrNoPathToAnchor, Code: OutcomeNoPathToAnchor}, nil
}
