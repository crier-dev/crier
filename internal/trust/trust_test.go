package trust

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tsString is the unix-seconds form timestamps take in transcripts.
func tsString(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// fixtureChain builds the acceptance fixture (task CR-CHAT-025, criterion e):
//
//	anchor (held by the verifier) --vouches--> delegate1 --vouches--> delegate2
//
// with anchor's delegation carrying max_depth 2 so the second hop is legal
// under D16 (§4.3). It returns the verifier's store plus every keypair.
func fixtureChain(t *testing.T) (store *Store, anchor, del1, del2 ed25519.PrivateKey, now time.Time) {
	t.Helper()
	now = time.Now().Truncate(time.Second)
	store = NewStore()

	gen := func() ed25519.PrivateKey {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		return priv
	}
	anchor = gen()
	del1 = gen()
	del2 = gen()

	// The anchor is the verifier's LOCAL decision (§3): accepted_by names the
	// operator, never "system".
	pub, _ := anchor.Public().(ed25519.PublicKey)
	err := store.AddAnchor(Anchor{
		ID:         "anchor_01J9Z8Q2K7",
		Subject:    Subject{Type: "instance", ID: "peer_acme"},
		PublicKey:  hexPub(pub),
		AcceptedAt: now,
		AcceptedBy: "prin_01J9Z6V0Q7",
	})
	if err != nil {
		t.Fatalf("AddAnchor: %v", err)
	}

	// anchor vouches for delegate1 with room for one further hop (D16).
	d1 := Delegation{
		ID:          "vouch_anchor_to_1",
		IssuerKey:   keyID(pub),
		SubjectKey:  keyID(mustPub(del1)),
		PublicKey:   hexPub(mustPub(del1)),
		SubjectType: "agent",
		Scope: Scope{
			Namespaces: []string{"acme-mirror"},
			Directions: []string{"inbound"},
			Purposes:   []string{"message-signing"},
			MaxDepth:   2,
		},
		NotBefore: now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&d1, anchor)
	if err := store.AddDelegation(d1); err != nil {
		t.Fatalf("AddDelegation(1): %v", err)
	}

	// delegate1 vouches for delegate2 (hop 1 of the subject chain, hop 2 of
	// the walk — allowed by the max_depth 2 above).
	d2 := Delegation{
		ID:          "vouch_1_to_2",
		IssuerKey:   keyID(mustPub(del1)),
		SubjectKey:  keyID(mustPub(del2)),
		PublicKey:   hexPub(mustPub(del2)),
		SubjectType: "agent",
		Scope: Scope{
			Namespaces: []string{"acme-mirror"},
			Directions: []string{"inbound"},
			Purposes:   []string{"message-signing"},
			MaxDepth:   2,
		},
		NotBefore: now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&d2, del1)
	if err := store.AddDelegation(d2); err != nil {
		t.Fatalf("AddDelegation(2): %v", err)
	}
	return store, anchor, del1, del2, now
}

func hexPub(pub ed25519.PublicKey) string { return hexEncode(pub) }
func mustPub(priv ed25519.PrivateKey) ed25519.PublicKey {
	return priv.Public().(ed25519.PublicKey)
}

// a) Signed message from an untrusted-but-vouched key verifies via a chain to
// a held anchor.
func TestVerifyViaChainToAnchor(t *testing.T) {
	store, _, _, del2, now := fixtureChain(t)
	payload := []byte("crier-trust-v1\npeer_acme\natlas-on-b\nmsg_1\ndeadbeef\n" +
		tsString(now))
	sig := ed25519.Sign(del2, payload)

	v := store.Verify(mustPub(del2), payload, sig, VerifyOptions{
		Namespace: "acme-mirror", Direction: "inbound", Purpose: "message-signing",
		SignedAt: now,
	})
	if v.Err != nil {
		t.Fatalf("vouched key must verify via chain to anchor: got %v (%s)", v.Err, v.Code)
	}
	if v.Code != "" || v.Class != TrustClassNone {
		t.Fatalf("clean verdict, got code=%q class=%q", v.Code, v.Class)
	}
}

// b) A key with NO path to an anchor is refused with a NAMED error.
func TestVerifyNoPathNamedError(t *testing.T) {
	store, _, _, _, now := fixtureChain(t)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	store.RegisterKey(mustPub(stranger)) // known-key set, but vouched by no one
	payload := []byte("untrusted")
	sig := ed25519.Sign(stranger, payload)

	v := store.Verify(mustPub(stranger), payload, sig, VerifyOptions{SignedAt: now})
	if !errors.Is(v.Err, ErrNoPathToAnchor) {
		t.Fatalf("want ErrNoPathToAnchor, got %v", v.Err)
	}
	if v.Code != OutcomeNoPathToAnchor {
		t.Fatalf("named outcome: want %s, got %q", OutcomeNoPathToAnchor, v.Code)
	}
}

// b') An out-of-scope use gets its OWN named outcome (§5.1 step 3).
func TestVerifyOutOfScopeNamed(t *testing.T) {
	store, _, _, del2, now := fixtureChain(t)
	payload := []byte("payload")
	sig := ed25519.Sign(del2, payload)
	v := store.Verify(mustPub(del2), payload, sig, VerifyOptions{
		Namespace: "acme-mirror", Direction: "inbound",
		Purpose:  "admin-bypass", // not covered by the delegation
		SignedAt: now,
	})
	if !errors.Is(v.Err, ErrDelegationOutOfScope) || v.Code != OutcomeDelegationOutOfScope {
		t.Fatalf("want out-of-scope verdict, got %v (%s)", v.Err, v.Code)
	}
}

// b”) An out-of-window use gets its OWN named outcome, never a clamp (§4.2).
func TestVerifyExpiredDelegationNamed(t *testing.T) {
	store, _, _, del2, now := fixtureChain(t)
	late := now.Add(2 * time.Hour) // past every delegation's expires_at
	payload := []byte("payload")
	sig := ed25519.Sign(del2, payload)
	v := store.Verify(mustPub(del2), payload, sig, VerifyOptions{SignedAt: late})
	if !errors.Is(v.Err, ErrDelegationExpired) || v.Code != OutcomeDelegationExpired {
		t.Fatalf("want expired verdict, got %v (%s)", v.Err, v.Code)
	}
}

// b”') A D16 violation: a chain longer than max_depth cannot vouch onward.
func TestVerifyMaxDepthZeroCannotDelegate(t *testing.T) {
	store, anchor, _, _, now := fixtureChain(t)
	_, mid, _ := ed25519.GenerateKey(rand.Reader)
	_, leaf, _ := ed25519.GenerateKey(rand.Reader)

	// anchor -> mid with max_depth 0: mid may sign, but may NOT vouch onward.
	d := Delegation{
		ID: "vouch_zero_depth", IssuerKey: keyID(mustPub(anchor)),
		SubjectKey: keyID(mustPub(mid)),
		PublicKey:  hexPub(mustPub(mid)), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 0},
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&d, anchor)
	if err := store.AddDelegation(d); err != nil {
		t.Fatalf("AddDelegation: %v", err)
	}
	// mid vouches for leaf anyway — the record signature is real, the
	// AUTHORITY is not.
	d2 := Delegation{
		ID: "vouch_beyond_depth", IssuerKey: keyID(mustPub(mid)),
		SubjectKey: keyID(mustPub(leaf)),
		PublicKey:  hexPub(mustPub(leaf)), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 2},
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&d2, mid)
	if err := store.AddDelegation(d2); err != nil {
		t.Fatalf("AddDelegation(d2): %v (mid IS held, signature verifies — import is fine)", err)
	}

	payload := []byte("payload")
	v := store.Verify(mustPub(leaf), payload, ed25519.Sign(leaf, payload), VerifyOptions{SignedAt: now})
	if !errors.Is(v.Err, ErrDelegationOutOfScope) || v.Code != OutcomeDelegationOutOfScope {
		t.Fatalf("D16: leaf beyond a max_depth 0 delegation must be refused (named), got %v (%s)", v.Err, v.Code)
	}
}

// b””) A tampered delegation is never imported — editing a scope breaks the
// issuer's signature (§4.1).
func TestTamperedDelegationRefused(t *testing.T) {
	store, _, del1, _, now := fixtureChain(t)
	// Forge a widened delegation in delegate1's name.
	forged := Delegation{
		ID: "forged", IssuerKey: keyID(mustPub(del1)),
		PublicKey: hexPub(mustPub(del1)), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 9},
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Signature: "00",
	}
	if err := store.AddDelegation(forged); err == nil {
		t.Fatal("a delegation with a malformed signature must not import")
	}
	// And a real signature over DIFFERENT bytes (wrong issuer) must not either.
	_, impostor, _ := ed25519.GenerateKey(rand.Reader)
	bad := Delegation{
		ID: "bad-issuer", IssuerKey: keyID(mustPub(del1)),
		SubjectKey: keyID(mustPub(impostor)),
		PublicKey:  hexPub(mustPub(impostor)), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 2},
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&bad, impostor) // signed by the SUBJECT, not the issuer
	if err := store.AddDelegation(bad); err == nil {
		t.Fatal("delegation not signed by its issuer must not import")
	}
}

// c) Rotating a key keeps the chain verifiable: old records verify under the
// old key, and the new key enters the chain through the rotation certificate
// signed by the OUTGOING key (§6).
func TestRotationKeepsChainVerifiable(t *testing.T) {
	store, _, del1, del2, now := fixtureChain(t)
	oldPub := mustPub(del2)
	payload := []byte("old record")
	oldSig := ed25519.Sign(del2, payload)

	// Old record verifies before rotation.
	if v := store.Verify(oldPub, payload, oldSig, VerifyOptions{SignedAt: now}); v.Err != nil {
		t.Fatalf("pre-rotation verify: %v", v.Err)
	}

	// Rotate: delegate2's holder generates a new key; the OUTGOING key signs
	// the certificate (§6.1).
	_, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	newPub := mustPub(newPriv)
	cert := RotationCert{
		OldKeyID: keyID(oldPub), NewKeyID: keyID(newPub),
		NotBefore: now.Add(time.Second), Reason: "scheduled rotation",
		NewPublicKey: hexPub(newPub),
	}
	SignRotationCert(&cert, del2)
	if err := store.AddRotation(cert); err != nil {
		t.Fatalf("AddRotation: %v", err)
	}
	// Sanity: the same record STILL verifies after rotation — history is not
	// rewritten (§6.2).
	if v := store.Verify(oldPub, payload, oldSig, VerifyOptions{SignedAt: now}); v.Err != nil {
		t.Fatalf("old record must keep verifying after rotation: %v", v.Err)
	}
	// The new key, presented in the old chain's place, is NOT trusted: the
	// chain still routes through delegate2, and nothing vouches for the new
	// key yet. Register it and prove §3.2.
	store.RegisterKey(newPub)
	newPayload := []byte("new record")
	v := store.Verify(newPub, newPayload, ed25519.Sign(newPriv, newPayload), VerifyOptions{SignedAt: now.Add(time.Minute)})
	if !errors.Is(v.Err, ErrNoPathToAnchor) {
		t.Fatalf("rotated key without a delegation is not yet trusted: got %v (%s)", v.Err, v.Code)
	}
	// The chain accepts the new key once a delegation routes through it —
	// here the (rotated) subject re-vouched by its voucher era: delegate1
	// vouches for the NEW key, closing the chain at the anchor again.
	dNew := Delegation{
		ID: "vouch_1_to_2_rotated", IssuerKey: keyID(mustPub(del1)),
		SubjectKey: keyID(newPub),
		PublicKey:  hexPub(newPub), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 2},
		NotBefore: now, ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&dNew, del1)
	if err := store.AddDelegation(dNew); err != nil {
		t.Fatalf("AddDelegation(new): %v", err)
	}
	v = store.Verify(newPub, newPayload, ed25519.Sign(newPriv, newPayload), VerifyOptions{SignedAt: now.Add(time.Minute)})
	if v.Err != nil {
		t.Fatalf("rotated key verifies via re-vouching: %v (%s)", v.Err, v.Code)
	}
}

// c') A rotation certificate NOT signed by the outgoing key is refused with
// the named outcome (§6.3).
func TestRotationUnsignedRefused(t *testing.T) {
	store, anchor, _, del2, now := fixtureChain(t)
	_, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	newPub := mustPub(newPriv)

	// Signed by the WRONG key (the anchor, not the outgoing delegate2).
	cert := RotationCert{
		OldKeyID: keyID(mustPub(del2)), NewKeyID: keyID(newPub),
		NotBefore: now, Reason: "r", NewPublicKey: hexPub(newPub),
	}
	SignRotationCert(&cert, anchor)
	err := store.AddRotation(cert)
	if !errors.Is(err, ErrRotationUnsigned) {
		t.Fatalf("want ErrRotationUnsigned, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), OutcomeRotationUnsigned) {
		t.Fatalf("error must name %s: %v", OutcomeRotationUnsigned, err)
	}
}

// d) Revoking a key: signatures AT/AFTER revocation are refused (named), and
// the meaning for OLD records is stated — a pre-revocation signature still
// VERIFIES, annotated revoked-after-signing (§7.3, §7.5).
func TestRevocationTimeAnchored(t *testing.T) {
	store, _, _, del2, now := fixtureChain(t)
	pub := mustPub(del2)
	payload := []byte("historical record")
	sig := ed25519.Sign(del2, payload)

	// Pre-revocation: clean verdict.
	if v := store.Verify(pub, payload, sig, VerifyOptions{SignedAt: now}); v.Err != nil {
		t.Fatalf("pre-revocation verify: %v", v.Err)
	}

	revokedAt := now.Add(time.Hour)
	store.Revoke(Revocation{KeyID: keyID(pub), RevokedAt: revokedAt, RevokedBy: "operator", Reason: "compromise"})

	// OLD record (signed before revoked_at): still verifies, with the trust
	// class. It is evidence of what happened — NOT a claim the key was
	// uncompromised (§7.5).
	v := store.Verify(pub, payload, sig, VerifyOptions{SignedAt: now})
	if v.Err != nil {
		t.Fatalf("pre-revocation record must keep verifying after revocation: %v", v.Err)
	}
	if v.Class != TrustClassRevokedAfterSigning || v.Code != OutcomeRevokedAfterSigning {
		t.Fatalf("want revoked-after-signing class, got code=%q class=%q", v.Code, v.Class)
	}

	// Signature AT the revocation instant: refused.
	v = store.Verify(pub, payload, sig, VerifyOptions{SignedAt: revokedAt})
	if !errors.Is(v.Err, ErrKeyRevoked) || v.Code != OutcomeKeyRevoked {
		t.Fatalf("signature at revoked_at must be refused, got %v (%s)", v.Err, v.Code)
	}
	// Signature AFTER: refused.
	v = store.Verify(pub, payload, sig, VerifyOptions{SignedAt: revokedAt.Add(time.Minute)})
	if !errors.Is(v.Err, ErrKeyRevoked) {
		t.Fatalf("signature after revoked_at must be refused, got %v", v.Err)
	}

	// Revoked key's FORWARD authority ends: it cannot import a new
	// delegation (its vouching signature is refused at import, §7.3).
	_, fresh, _ := ed25519.GenerateKey(rand.Reader)
	d := Delegation{
		ID: "vouch_by_revoked", IssuerKey: keyID(pub),
		SubjectKey: keyID(mustPub(fresh)),
		PublicKey:  hexPub(mustPub(fresh)), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 0},
		NotBefore: revokedAt.Add(time.Minute), ExpiresAt: revokedAt.Add(time.Hour),
	}
	SignDelegation(&d, del2)
	if err := store.AddDelegation(d); err == nil {
		t.Fatal("a revoked key must not vouch onward (forward authority ended)")
	}
}

// d') Revocation anywhere on the PATH refuses outright — a revoked voucher
// cannot reprieve itself by having signed before revocation (§5.1 step 5).
func TestRevokedVoucherRefusesDescendants(t *testing.T) {
	store, _, del1, del2, now := fixtureChain(t)
	store.Revoke(Revocation{KeyID: keyID(mustPub(del1)), RevokedAt: now, RevokedBy: "op", Reason: "voucher compromised"})
	payload := []byte("payload")
	v := store.Verify(mustPub(del2), payload, ed25519.Sign(del2, payload), VerifyOptions{SignedAt: now.Add(time.Minute)})
	if !errors.Is(v.Err, ErrKeyRevoked) {
		t.Fatalf("descendant of a revoked voucher must be refused, got %v (%s)", v.Err, v.Code)
	}
}

// §3.2: an expired anchor is treated as ABSENT, not deleted.
func TestExpiredAnchorInert(t *testing.T) {
	_, anchor, del1, _, now := fixtureChain(t)
	// Rebuild the anchor with an expiry in the past.
	expired := now.Add(-time.Hour)
	s2 := NewStore()
	pub := mustPub(anchor)
	if err := s2.AddAnchor(Anchor{
		ID: "anchor_exp", Subject: Subject{Type: "instance", ID: "peer_acme"},
		PublicKey: hexPub(pub), AcceptedAt: now.Add(-2 * time.Hour),
		AcceptedBy: "operator", ExpiresAt: &expired,
	}); err != nil {
		t.Fatalf("AddAnchor: %v", err)
	}
	// Re-import delegate1's delegation into the new store.
	d1 := Delegation{
		ID: "vouch_anchor_to_1_b", IssuerKey: keyID(pub),
		SubjectKey: keyID(mustPub(del1)),
		PublicKey:  hexPub(mustPub(del1)), SubjectType: "agent",
		Scope:     Scope{MaxDepth: 2},
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	SignDelegation(&d1, anchor)
	if err := s2.AddDelegation(d1); err != nil {
		t.Fatalf("AddDelegation: %v", err)
	}
	payload := []byte("payload")
	v := s2.Verify(mustPub(del1), payload, ed25519.Sign(del1, payload), VerifyOptions{SignedAt: now})
	if !errors.Is(v.Err, ErrNoPathToAnchor) {
		t.Fatalf("expired anchor must be treated as absent, got %v (%s)", v.Err, v.Code)
	}
}

// §5.1 step 6 ordering: the chain is walked BEFORE the signature is verified —
// an invalid signature from an UNTRUSTED key reports no-path, not
// signature-invalid (separate verdicts, separate codes).
func TestChainWalkBeforeSignature(t *testing.T) {
	store, _, _, _, now := fixtureChain(t)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	store.RegisterKey(mustPub(stranger))
	v := store.Verify(mustPub(stranger), []byte("p"), []byte("not-a-real-signature-but-64-bytes-long-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), VerifyOptions{SignedAt: now})
	if !errors.Is(v.Err, ErrNoPathToAnchor) {
		t.Fatalf("untrusted key must be refused at the chain, not the signature: got %v (%s)", v.Err, v.Code)
	}
	// And a trusted key with a broken signature gets signature-invalid.
	store2, _, _, d2, n2 := fixtureChain(t)
	v = store2.Verify(mustPub(d2), []byte("p"), []byte("broken"), VerifyOptions{SignedAt: n2})
	if !errors.Is(v.Err, ErrSignatureInvalid) || v.Code != OutcomeSignatureInvalid {
		t.Fatalf("trusted key with bad signature: want %s, got %v (%s)", OutcomeSignatureInvalid, v.Err, v.Code)
	}
}
