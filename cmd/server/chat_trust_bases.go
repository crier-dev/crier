package main

import "github.com/crier-dev/crier/internal/trust"

// chatTrustBasisWords is the panel's single explanation table. Keys are the
// verifier's constants, not another copy of its wire vocabulary. The served
// script is rendered from this table; tests require every TrustBasis constant
// to have a distinct sentence and compare the complete served map with it.
var chatTrustBasisWords = map[string]string{
	trust.TrustBasisChainOnly:           "Chain reachability only; no eligible possession challenge.",
	trust.TrustBasisPossessionVerified:  "Leaf proof verified and current registry key matches.",
	trust.TrustBasisRegistryKeyMismatch: "Leaf proof verified, but the current registered key differs.",
	trust.TrustBasisInvalidSignature:    "A well-formed possession proof failed leaf-key signature verification.",
	trust.TrustBasisStaleTimestamp:      "The proof timestamp is outside the allowed freshness window.",
	trust.TrustBasisNoRegistryKey:       "The identified agent is unknown or has no usable registered signing key.",
	trust.TrustBasisMalformed:           "The possession proof could not be parsed or read in the required format.",
	trust.TrustBasisMissing:             "A required proof identity, timestamp or signature is missing or empty.",
	trust.TrustBasisBodyUnbound:         "The required request-body digest is missing or empty.",
	trust.TrustBasisBodyMismatch:        "The supplied digest does not match the exact request-body bytes.",
	trust.TrustBasisChallengeOversized:  "The possession challenge exceeds the allowed byte limit.",
	trust.TrustBasisRegistryUnavailable: "The registry lookup failed; this does not establish forgery or a missing key.",
}
