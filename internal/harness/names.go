package harness

import (
	"fmt"
	"sort"
	"strings"
)

// adapterAliases maps a spelling an operator will plausibly write onto the
// adapter id that implements it. An alias is a NAME for an existing adapter,
// never a second implementation: `pi-agent` and `pi` reach the same code, the
// same session table and the same metric label.
//
// `pi-agent` is here because the harness is published under that name by the
// code-factory installers and the node seed (scripts/harness/install-pi.sh,
// config/node-seed.txt), so an operator copying those docs into a crier config
// wrote a name crier refused — our own documentation failing against our own
// product. The canonical id stays `pi` because it is the binary's name and it is
// already the metric label and the persisted session binding.
var adapterAliases = map[string]string{
	"pi-agent": AdapterPi,
}

// CanonicalName resolves a configured or requested harness name to the adapter
// id that serves it. ok is false when nothing serves the name — which is a
// refusal, not a fallback: a delivery must never be diverted to a different
// harness because the requested one was misspelled.
func CanonicalName(name string) (string, bool) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", false
	}
	if isKnownAdapter(n) {
		return n, true
	}
	if canonical, ok := adapterAliases[n]; ok {
		return canonical, true
	}
	// An alias is only useful if its case does not have to be remembered:
	// "Pi-Agent" is the same request as "pi-agent".
	if canonical, ok := adapterAliases[strings.ToLower(n)]; ok {
		return canonical, true
	}
	return "", false
}

// AcceptedNames is the full set a config or a caller may name: the adapter ids
// and the aliases, sorted, with each alias shown against the id it resolves to.
// It is derived from KnownAdapters so a new adapter cannot be added without
// appearing here.
func AcceptedNames() []string {
	out := make([]string, 0, len(KnownAdapters)+len(adapterAliases))
	out = append(out, KnownAdapters...)
	for alias := range adapterAliases {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// AcceptedNamesHelp renders the accepted set for an error message, naming each
// alias with the adapter it reaches so an operator does not have to guess
// whether `pi-agent` is a second harness.
func AcceptedNamesHelp() string {
	parts := make([]string, 0, len(KnownAdapters)+len(adapterAliases))
	parts = append(parts, KnownAdapters...)
	aliases := make([]string, 0, len(adapterAliases))
	for alias := range adapterAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		parts = append(parts, fmt.Sprintf("%s (alias of %s)", alias, adapterAliases[alias]))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// Suggest returns the known name an unknown one most plausibly meant, or "".
// The comparison ignores case and the punctuation that separates words, so
// "pi_agent", "PiAgent" and "pi agent" all point at "pi-agent" — a warning with
// the intended match is worth more than a bare rejection.
func Suggest(name string) string {
	want := normaliseName(name)
	if want == "" {
		return ""
	}
	candidates := append([]string{}, KnownAdapters...)
	for alias := range adapterAliases {
		candidates = append(candidates, alias)
	}
	sort.Strings(candidates)
	for _, c := range candidates {
		if normaliseName(c) == want {
			return c
		}
	}
	return ""
}

// normaliseName strips everything that cannot carry meaning in a harness name.
func normaliseName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case '-', '_', ' ', '.', '/':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
