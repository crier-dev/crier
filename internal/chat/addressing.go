package chat

import (
	"regexp"
	"strings"
)

// addresseePattern matches an @agent token: @ followed by word characters
// limited to [A-Za-z0-9._-], stopping at whitespace or punctuation.
var addresseePattern = regexp.MustCompile(`@([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*)`)

// ParseAddressees extracts the @agent tokens from a body, in order of
// appearance, without duplicates. Empty for a body with no tags.
func ParseAddressees(body string) []string {
	matches := addresseePattern.FindAllStringSubmatch(body, -1)
	var out []string
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

// Classify derives the effective kind of a message. An explicit Kind of task
// always wins (a TASK is only ever created deliberately — tagging alone is
// never enough); otherwise tags in the body make it ADDRESSED; otherwise it
// is PLAIN.
//
// The rule encoded here — and tested — is that an ADDRESSED message whose
// body reads like an instruction ("please fix X") is STILL just an addressed
// message. Nothing in this package executes anything; only a TASK may create
// work, and only via an explicit kind.
func Classify(body string, explicit Kind) (Kind, error) {
	switch explicit {
	case KindTask, KindPlain, KindAddressed:
		return explicit, nil
	case "":
		// No explicit kind: derive from the body.
	default:
		return "", ErrInvalidKind
	}
	if len(ParseAddressees(body)) > 0 {
		return KindAddressed, nil
	}
	return KindPlain, nil
}

// EnsureUnique returns the addressee list with duplicates removed and
// whitespace trimmed; empty tokens are dropped. Helper for callers that
// collect addressees from free-form input.
func EnsureUnique(addressees []string) []string {
	var out []string
	seen := make(map[string]bool, len(addressees))
	for _, a := range addressees {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}
