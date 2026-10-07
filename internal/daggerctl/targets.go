package daggerctl

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrUnknownTarget is a create that named a target the configured table does
// not hold (CR-CHAT-035). It is a NAMED refusal at the API boundary, before
// the bridge is touched: a run recorded against a target that cannot be
// addressed is a run whose status could never be observed again, so it is
// never created.
var ErrUnknownTarget = errors.New("unknown dagger execution target")

// TargetTable maps execution-target names onto the base URLs crier talks to
// for them (CR-CHAT-035). The default target is ALWAYS present, named
// TargetLocal, and points at the CR_DAGGER_URL bridge; remote targets come
// from CR_DAGGER_TARGET_<NAME> and are keyed by their lowercase name. The
// table is resolved at create time and FROZEN onto the record — a later
// status read uses the record's target, never a re-resolved one.
type TargetTable struct {
	// defaultURL is the local bridge's base URL (CR_DAGGER_URL).
	defaultURL string
	// remotes maps a lowercase target name to its base URL.
	remotes map[string]string
}

// NewTargetTable builds the table: one default (the CR_DAGGER_URL bridge) plus
// the named remote entries. An entry whose URL is empty or not absolute is
// refused here, so a misconfigured CR_DAGGER_TARGET_<NAME> fails at startup,
// exactly as a bad CR_DAGGER_URL always has.
func NewTargetTable(defaultURL string, remotes map[string]string) (*TargetTable, error) {
	t := &TargetTable{defaultURL: strings.TrimSpace(defaultURL), remotes: map[string]string{}}
	for name, raw := range remotes {
		u := strings.TrimSpace(raw)
		if err := validateTargetURL(u); err != nil {
			return nil, fmt.Errorf("target %q: %w", name, err)
		}
		t.remotes[strings.ToLower(strings.TrimSpace(name))] = u
	}
	return t, nil
}

// validateTargetURL reuses the bridge's own URL grammar.
func validateTargetURL(raw string) error {
	_, err := NewHTTPBridge(raw)
	return err
}

// Resolve maps a requested target name onto a base URL. An empty name is the
// default (local) target; an unlisted name is ErrUnknownTarget, wrapped with
// the name the caller used so the refusal names the fix.
func (t *TargetTable) Resolve(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || n == TargetLocal {
		if t.defaultURL == "" {
			return "", fmt.Errorf("%w: no default dagger bridge is configured (set CR_DAGGER_URL)", ErrUnconfigured)
		}
		return t.defaultURL, nil
	}
	u, ok := t.remotes[n]
	if !ok {
		return "", fmt.Errorf("%w: %q (configured targets: %s)", ErrUnknownTarget, name, t.Names())
	}
	return u, nil
}

// Names returns the configured target names, default first, sorted — for
// error text and operator-facing output.
func (t *TargetTable) Names() []string {
	names := []string{TargetLocal}
	var remotes []string
	for n := range t.remotes {
		remotes = append(remotes, n)
	}
	sort.Strings(remotes)
	return append(names, remotes...)
}

// remote reports whether the given (already recorded) target name names a
// REMOTE execution target. Only remote runs can go link-lost: a local run
// that cannot be observed is a bridge failure the caller sees directly, not a
// lost link to a box crier cannot see past.
func (t *TargetTable) remote(name string) bool {
	_, ok := t.remotes[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// remoteBridge returns the bridge for a recorded REMOTE target name. The
// name must already be in the table (it was resolved at create time); a
// name that is not is a programming error and answers ErrUnknownTarget.
func (t *TargetTable) remoteBridge(name string) (DaggerBridge, error) {
	u, err := t.Resolve(name)
	if err != nil {
		return nil, err
	}
	return NewHTTPBridge(u)
}

// BridgeFor returns the bridge that speaks to a recorded target (CR-CHAT-035):
// the default bridge for the local target, a per-target bridge for a remote
// one. The interface from crier is the SAME bridge vocabulary either way —
// only the execution target differs.
func (t *TargetTable) BridgeFor(recordedTarget string, fallback DaggerBridge) (DaggerBridge, error) {
	if !t.remote(recordedTarget) {
		return fallback, nil
	}
	return t.remoteBridge(recordedTarget)
}
