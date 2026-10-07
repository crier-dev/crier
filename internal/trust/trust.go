// Package trust implements the signing trust model of specs/CHAT-TRUST.md
// (CR-CHAT-025): trust anchors (§3), delegation/vouching records (§4), the
// end-to-end verification walk with NAMED outcomes (§5), key rotation without
// breaking verifiable history (§6), and revocation as time-anchored trust
// (§7). It is a LIBRARY, not an HTTP surface: the /trust/* routes of §9 item 4
// remain unbuilt, and nothing here changes the shipped request-signature path
// in internal/registry (request-line verification keeps working unchanged).
//
// The one question this package answers (spec §1.1): a signature arrives made
// by a key the verifier has never seen — should it be believed, and what
// exactly is believed? Trust starts at an ANCHOR and nowhere else (§3.2); a
// key is believed only if it IS an anchor or a bounded chain of signed
// delegations leads to one, with every hop in scope, in its temporal window,
// and unrevoked at the relevant instant.
package trust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// TrustClass is the temporal annotation a verdict carries for a revoked
// signer (spec §7.3): a signature that verified BEFORE revocation is a
// different verdict from one refused AT/AFTER it.
type TrustClass string

const (
	// TrustClassNone: the key was not revoked at the relevant instant.
	TrustClassNone TrustClass = ""
	// TrustClassRevokedAfterSigning: the signature was made while the key
	// was still trusted, and the key has since been revoked. The signature
	// VERIFIES and is suspect at once (§7.5): genuine, historical, and not
	// evidence the key was uncompromised.
	TrustClassRevokedAfterSigning TrustClass = "revoked-after-signing"
)

// Named outcomes of a verification (spec §5.1, §6.3, §7.3). Each is the code
// a caller maps to its own HTTP surface; the library itself never writes a
// response, so the statuses here are the RECOMMENDED ones from the spec's
// worked example (§5.1 table).
const (
	OutcomeMalformed              = "TRUST_MALFORMED"                // 400
	OutcomeNoPathToAnchor         = "TRUST_NO_PATH_TO_ANCHOR"        // 403
	OutcomeDelegationOutOfScope   = "TRUST_DELEGATION_OUT_OF_SCOPE"  // 403
	OutcomeDelegationExpired      = "TRUST_DELEGATION_EXPIRED"       // 403
	OutcomeKeyRevoked             = "TRUST_KEY_REVOKED"              // 403
	OutcomeSignatureInvalid       = "TRUST_SIGNATURE_INVALID"        // 401
	OutcomeReplayWindow           = "TRUST_REPLAY_WINDOW"            // 401
	OutcomeRevokedAfterSigning    = "TRUST_REVOKED_AFTER_SIGNING"    // verified, suspect
	OutcomeRotationUnsigned       = "TRUST_ROTATION_UNSIGNED"        // 403
	OutcomeRotationOutsideOverlap = "TRUST_ROTATION_OUTSIDE_OVERLAP" // 401/403
)

// Sentinel errors (spec §4, §5, §6.3, §7). Callers test with errors.Is; each
// carries its named outcome code in the message so a log line names the
// verdict without a lookup.
var (
	// ErrNoPathToAnchor: no delegation path leads from the presented key to
	// a held anchor within depth (§5.1 step 2) — the named refusal for a key
	// with no path.
	ErrNoPathToAnchor = fmt.Errorf("trust: key has no delegation path to a held anchor (%s)", OutcomeNoPathToAnchor)
	// ErrDelegationOutOfScope: a hop on the chain does not cover the use
	// (namespace / direction / purpose / depth) (§5.1 step 3).
	ErrDelegationOutOfScope = fmt.Errorf("trust: delegation out of scope for this use (%s)", OutcomeDelegationOutOfScope)
	// ErrDelegationExpired: a hop is outside its not_before/expires_at
	// window — refused, never clamped (§4.2, §5.1 step 4).
	ErrDelegationExpired = fmt.Errorf("trust: delegation outside its validity window (%s)", OutcomeDelegationExpired)
	// ErrKeyRevoked: a key in the path was revoked at or before the
	// relevant instant (§5.1 step 5, §7.3).
	ErrKeyRevoked = fmt.Errorf("trust: key is revoked (%s)", OutcomeKeyRevoked)
	// ErrSignatureInvalid: the signature does not verify over the payload
	// with the signer's key (§5.1 step 6).
	ErrSignatureInvalid = fmt.Errorf("trust: signature does not verify (%s)", OutcomeSignatureInvalid)
	// ErrMalformed: the envelope does not parse — a required field is
	// missing or ill-formed (§5.1 step 0).
	ErrMalformed = fmt.Errorf("trust: verification request is malformed (%s)", OutcomeMalformed)
	// ErrAnchorExpired: the terminal anchor is past its expires_at, and §3.1
	// treats it as absent — inert, not deleted.
	ErrAnchorExpired = fmt.Errorf("trust: anchor has expired and is treated as absent (%s)", OutcomeNoPathToAnchor)
	// ErrRotationUnsigned: a rotation certificate is not signed by the
	// OUTGOING key, so the incoming key is not trusted through it (§6.1).
	ErrRotationUnsigned = fmt.Errorf("trust: rotation certificate not signed by the outgoing key (%s)", OutcomeRotationUnsigned)
)

// keyID is the hex prefix of the sha256 of a public key — the shipped
// detect.Signer convention (internal/detect/audit.go), so a verifier can name
// WHICH key signed, here and in the delivery log, with one definition.
func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Subject is the thing a key speaks for (spec glossary): an instance or an
// agent. A key never speaks for "everyone" (§3.1).
type Subject struct {
	Type string `json:"type"` // "instance" | "agent"
	ID   string `json:"id"`
}

// Scope is what a delegation covers (§4.1). An EMPTY list is a wildcard for
// that dimension — a delegation that names no namespaces covers every
// namespace it is presented in — because a vouching tool that only means
// "this key may sign" should not have to enumerate the world. A verifier that
// wants narrow trust requires populated scopes in its LOCAL policy (§5.3),
// which is a separate bound from the chain walk.
type Scope struct {
	Namespaces []string `json:"namespaces,omitempty"`
	Directions []string `json:"directions,omitempty"`
	Purposes   []string `json:"purposes,omitempty"`
	// MaxDepth limits how many FURTHER delegations this delegation's subject
	// may issue. Zero vouches for a signing key and CANNOT vouch onward —
	// decision D16's "direct peers only by default" (§4.2, §4.3).
	MaxDepth int `json:"max_depth"`
}

// covers reports whether the scope admits one use.
func (s Scope) covers(namespace, direction, purpose string) bool {
	return coversList(s.Namespaces, namespace) &&
		coversList(s.Directions, direction) &&
		coversList(s.Purposes, purpose)
}

func coversList(list []string, want string) bool {
	if want == "" {
		return true // an unspecified dimension makes no claim
	}
	if len(list) == 0 {
		return true // wildcard (documented above)
	}
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Anchor is a key a verifier already believes, by LOCAL decision, before any
// signature arrives (§3). accepted_by is never "system": a machine did not
// decide to trust an instance. An expired anchor is inert, not deleted (§3.1).
type Anchor struct {
	ID         string     `json:"id"`
	Subject    Subject    `json:"subject"`
	KeyID      string     `json:"key_id"`
	PublicKey  string     `json:"public_key"` // hex ed25519 public key
	AcceptedAt time.Time  `json:"accepted_at"`
	AcceptedBy string     `json:"accepted_by"`
	ExpiresAt  *time.Time `json:"expires_at"` // nil = no expiry
}

// Delegation is one voucher signature over a subject key (§4.1): the issuer
// signs the canonical record (every field except Signature, deterministically
// encoded — the same rule detect.Entry.canonical() uses), so a delegation
// cannot be edited into a wider scope.
type Delegation struct {
	ID          string    `json:"id"`
	IssuerKey   string    `json:"issuer_key_id"`
	SubjectKey  string    `json:"subject_key_id"`
	PublicKey   string    `json:"public_key"` // hex ed25519 public key of the subject
	SubjectType string    `json:"subject_type"`
	Scope       Scope     `json:"scope"`
	NotBefore   time.Time `json:"not_before"`
	ExpiresAt   time.Time `json:"expires_at"`
	Signature   string    `json:"signature"` // hex ed25519 sig by the issuer over canonical()
}

// canonical returns the bytes the issuer signs: the record with its own
// Signature blanked, JSON-encoded deterministically (Go marshals struct
// fields in declaration order — the detect.Entry rule).
func (d Delegation) canonical() []byte {
	d.Signature = ""
	raw, err := json.Marshal(d)
	if err != nil {
		// Unreachable: every field is a string, an int or a time.
		panic(fmt.Sprintf("trust: canonical delegation: %v", err))
	}
	return raw
}

// SignDelegation fills d.Signature with the issuer's ed25519 signature over
// the canonical bytes — the ONE function that builds a vouching signature, so
// the wire contract cannot drift between the vouching tool and the verifier.
func SignDelegation(d *Delegation, issuer ed25519.PrivateKey) {
	sig := ed25519.Sign(issuer, d.canonical())
	d.Signature = hex.EncodeToString(sig)
}

// RotationCert is the append record that links a key's eras (§6.1): the
// OUTGOING key signs the transcript
//
//	crier-trust-rotate-v1\n<old_key_id>\n<new_key_id>\n<not_before>\n<reason>
//
// The incoming key is not trusted through the rotation until the outgoing key
// has signed it. Re-signing old records is NOT rotation and is not supported
// by anything here — it is forgery (§6.2).
type RotationCert struct {
	OldKeyID  string    `json:"old_key_id"`
	NewKeyID  string    `json:"new_key_id"`
	NotBefore time.Time `json:"not_before"`
	Reason    string    `json:"reason"`
	Signature string    `json:"signature"` // hex ed25519 sig by the OLD key
	// NewPublicKey is the incoming key's hex bytes, carried for the
	// verifier's own resolution. It is NOT part of the signed transcript —
	// the transcript names the key by id, and the bytes are pinned by that
	// id being the sha256 prefix of whatever key the verifier registers.
	NewPublicKey string `json:"new_public_key"`
}

// rotationTranscript is the ONE function that builds the signed bytes, so the
// wire contract cannot drift between the signer and the verifier.
func (c RotationCert) rotationTranscript() []byte {
	return []byte("crier-trust-rotate-v1\n" + c.OldKeyID + "\n" + c.NewKeyID + "\n" +
		c.NotBefore.UTC().Format(time.RFC3339) + "\n" + c.Reason)
}

// SignRotationCert fills c.Signature with the OUTGOING key's signature over
// the transcript.
func SignRotationCert(c *RotationCert, oldKey ed25519.PrivateKey) {
	sig := ed25519.Sign(oldKey, c.rotationTranscript())
	c.Signature = hex.EncodeToString(sig)
}

// Revocation is a tombstone: it is appended, never edited away (§7.4). It
// means what §7.5 says — records signed before RevokedAt keep their verdicts
// annotated revoked-after-signing; signatures at or after it are refused; the
// key's forward authority (rotations, new delegations) ends at it.
type Revocation struct {
	KeyID     string    `json:"key_id"`
	RevokedAt time.Time `json:"revoked_at"`
	RevokedBy string    `json:"revoked_by"`
	Reason    string    `json:"reason"`
}
