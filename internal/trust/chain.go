package trust

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The capability-chain surface is the first persistent slice of CHAT-TRUST
// §§3–5 (DF-CRIER-362). It does not authenticate a payload or use the library's
// rotation/revocation state. Those remain separate, unwired capabilities.
const (
	TrustVerified                 = "TRUST_VERIFIED"
	TrustChainReachable           = "TRUST_CHAIN_REACHABLE"
	TrustBasisChainOnly           = "chain-only"
	TrustBasisPossessionVerified  = "possession-verified"
	TrustBasisRegistryKeyMismatch = "proof-registry-key-mismatch"
	TrustBasisInvalidSignature    = "proof-invalid-signature"
	TrustBasisStaleTimestamp      = "proof-stale-timestamp"
	TrustBasisNoRegistryKey       = "proof-no-registry-key"
	TrustBasisMalformed           = "proof-malformed"
	TrustBasisMissing             = "proof-missing"
	TrustBasisBodyUnbound         = "proof-body-unbound"
	TrustBasisBodyMismatch        = "proof-body-mismatch"
	TrustBasisChallengeOversized  = "proof-challenge-oversized"
	TrustBasisRegistryUnavailable = "proof-registry-unavailable"
	TrustUnknownAnchor            = "TRUST_UNKNOWN_ANCHOR"
	TrustDepthExceeded            = "TRUST_DEPTH_EXCEEDED"
	TrustScopeInsufficient        = "TRUST_SCOPE_INSUFFICIENT"
	TrustBadSignature             = "TRUST_BAD_SIGNATURE"
	// TransitiveCapability is the explicit transitive purpose required by D16.
	// max_depth alone is a budget, not permission to vouch onward.
	TransitiveCapability = "trust-delegation"
)

// ChainAnchor is a locally accepted key, never supplied by a verify request.
// Creation is an operator-only action. Existing IDs and keys are immutable.
type ChainAnchor struct {
	ID        string    `json:"id"`
	PublicKey string    `json:"public_key"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// ChainDelegation uses the capability-list projection requested by DF-CRIER-362.
// Voucher is an anchor ID for the root link, and ChainKeyID of the preceding
// subject for subsequent links. max_depth counts FURTHER links after this one:
// zero allows a signing leaf, exactly as CHAT-TRUST §4.2 requires.
type ChainDelegation struct {
	Voucher    string    `json:"voucher"`
	SubjectKey string    `json:"subject_key"`
	Scope      []string  `json:"scope"`
	MaxDepth   int       `json:"max_depth"`
	IssuedAt   time.Time `json:"issued_at"`
	Signature  string    `json:"signature"`
}

// CanonicalBytes follows detect.Entry's deterministic struct-JSON convention:
// declaration order, signature blanked (not omitted), all other fields retained.
// Scope order and timestamp precision/offset are signed and never rewritten.
func (d ChainDelegation) CanonicalBytes() []byte {
	d.Signature = ""
	b, err := json.Marshal(d)
	if err != nil {
		panic(fmt.Sprintf("trust: canonical capability delegation: %v", err))
	}
	return b
}

func SignChainDelegation(d *ChainDelegation, voucher ed25519.PrivateKey) {
	d.Signature = hex.EncodeToString(ed25519.Sign(voucher, d.CanonicalBytes()))
}

// ChainKeyID shares the existing trust/detect sha256-prefix key identifier.
func ChainKeyID(publicKey string) string {
	pub, err := decodeKey(publicKey)
	if err != nil {
		return ""
	}
	return keyID(pub)
}

func (a ChainAnchor) Validate() error {
	// PostgreSQL text/JSONB cannot represent NUL; refuse it before storage on
	// every backend rather than allowing SQLite/memory-only trust records.
	if strings.ContainsRune(a.ID, 0) || strings.ContainsRune(a.Label, 0) {
		return fmt.Errorf("anchor id and label must not contain NUL")
	}
	if strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.Label) == "" || a.CreatedAt.IsZero() {
		return fmt.Errorf("anchor id, label and created_at are required")
	}
	_, err := decodeKey(a.PublicKey)
	return err
}

func (d ChainDelegation) Validate() error {
	if strings.ContainsRune(d.Voucher, 0) {
		return fmt.Errorf("voucher must not contain NUL")
	}
	if d.Scope == nil {
		return fmt.Errorf("scope must be a capability list (use [] for no capabilities)")
	}
	if strings.TrimSpace(d.Voucher) == "" || d.IssuedAt.IsZero() || d.MaxDepth < 0 || d.MaxDepth > DefaultMaxChainDepth {
		return fmt.Errorf("voucher, issued_at and max_depth in [0,%d] are required", DefaultMaxChainDepth)
	}
	if _, err := decodeKey(d.SubjectKey); err != nil {
		return err
	}
	if err := ValidateCapabilities(d.Scope); err != nil {
		return err
	}
	sig, err := hex.DecodeString(d.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature must be 64-byte hex ed25519")
	}
	return nil
}

// An empty list grants no capabilities; it is never a wildcard.
func ValidateCapabilities(scope []string) error {
	seen := map[string]bool{}
	for _, capability := range scope {
		if strings.ContainsRune(capability, 0) {
			return fmt.Errorf("scope capabilities must not contain NUL")
		}
		if strings.TrimSpace(capability) == "" || seen[capability] {
			return fmt.Errorf("scope capabilities must be nonempty and unique")
		}
		seen[capability] = true
	}
	return nil
}

func ValidateChainRequest(signer string, scope []string, chain []ChainDelegation) error {
	if _, err := decodeKey(signer); err != nil {
		return err
	}
	if err := ValidateCapabilities(scope); err != nil {
		return err
	}
	for i, d := range chain {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("link %d: %w", i+1, err)
		}
	}
	return nil
}

// ChainVerdict names every outcome. Link is ONE-BASED in anchor-to-leaf order.
// Depth counts links used, not the root key. VerifyChain only establishes chain
// reachability. The HTTP adapter may promote it after a body-bound leaf-key proof;
// neither verdict grants delivery authorization or asserts unrevoked status.
type ChainVerdict struct {
	Verdict           string `json:"verdict"`
	Basis             string `json:"basis,omitempty"`
	AnchorID          string `json:"anchor_id,omitempty"`
	Depth             int    `json:"depth"`
	Link              int    `json:"link,omitempty"`
	MissingCapability string `json:"missing_capability,omitempty"`

	// Signed server-minus-client clock skew, only for a stale possession proof.
	TimestampSkewSeconds *float64 `json:"timestamp_skew_seconds,omitempty"`
}

// VerifyChain verifies a caller-supplied PATH (not an unordered certificate set)
// against locally held anchors. Signatures are checked each time, not trusted
// because an import once succeeded. No peer can introduce its own anchor.
// A successful path is only TRUST_CHAIN_REACHABLE, never proof of possession.
func VerifyChain(anchors []ChainAnchor, signer string, scope []string, chain []ChainDelegation) ChainVerdict {
	unknown := ChainVerdict{Verdict: TrustUnknownAnchor}
	var anchor *ChainAnchor
	for i := range anchors {
		a := &anchors[i]
		if a.Validate() != nil {
			continue
		}
		if (len(chain) == 0 && sameChainKey(a.PublicKey, signer)) || (len(chain) > 0 && a.ID == chain[0].Voucher) {
			anchor = a
			break
		}
	}
	if anchor == nil {
		return unknown
	}
	if len(chain) > DefaultMaxChainDepth {
		return ChainVerdict{Verdict: TrustDepthExceeded, Link: DefaultMaxChainDepth + 1}
	}
	current := anchor.PublicKey
	seen := map[string]bool{ChainKeyID(current): true}
	for i, d := range chain {
		bad := ChainVerdict{Verdict: TrustBadSignature, Link: i + 1}
		pub, err := decodeKey(current)
		if err != nil {
			return bad
		}
		if i > 0 && d.Voucher != ChainKeyID(current) {
			return bad
		}
		if d.Validate() != nil {
			return bad
		}
		sig, _ := hex.DecodeString(d.Signature)
		if !ed25519.Verify(pub, d.CanonicalBytes(), sig) {
			return bad
		}
		if seen[ChainKeyID(d.SubjectKey)] {
			return bad
		}
		seen[ChainKeyID(d.SubjectKey)] = true
		remaining := len(chain) - i - 1
		if remaining > d.MaxDepth {
			return ChainVerdict{Verdict: TrustDepthExceeded, Link: i + 1}
		}
		for _, capability := range scope {
			if !chainScopeHas(d.Scope, capability) {
				return ChainVerdict{Verdict: TrustScopeInsufficient, Link: i + 1, MissingCapability: capability}
			}
		}
		if remaining > 0 && !chainScopeHas(d.Scope, TransitiveCapability) {
			return ChainVerdict{Verdict: TrustScopeInsufficient, Link: i + 1, MissingCapability: TransitiveCapability}
		}
		current = d.SubjectKey
	}
	if !sameChainKey(current, signer) {
		return ChainVerdict{Verdict: TrustBadSignature, Link: len(chain)}
	}
	return ChainVerdict{Verdict: TrustChainReachable, Basis: TrustBasisChainOnly, AnchorID: anchor.ID, Depth: len(chain)}
}

func sameChainKey(a, b string) bool { return ChainKeyID(a) != "" && strings.EqualFold(a, b) }
func chainScopeHas(scope []string, want string) bool {
	for _, s := range scope {
		if s == want {
			return true
		}
	}
	return false
}
