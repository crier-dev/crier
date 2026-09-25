// namespaces.go — building the realm policy set at boot (CR-FEAT-029).
//
// One resolution point, so the relay lane and the registry lane can never be
// given different policies: cmd/server builds the registry here and hands the
// SAME pointer to both. The alternative — each lane parsing the environment
// itself — is how two lanes drift into disagreeing about which realm a message
// belongs to.
package main

import (
	"strings"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/namespace"
)

// buildNamespaces resolves the namespace policy set from configuration.
//
//   - CR_NAMESPACES_FILE set → that file is the document (a missing or broken
//     file is an ERROR: a declared realm the process cannot read must not be
//     served as "no realms", which would silently put every agent back in the
//     default realm);
//   - CR_NAMESPACES set → that document, inline;
//   - neither → a registry with the default namespace and nothing else. That is
//     not a special case in any lane: every namespace-aware path resolves ""
//     to the default policy, whose unset fields mean "the deployment's
//     setting", so the server is byte-for-byte the one it was before this
//     feature existed.
//
// config.Load has already refused the both-set case, so the precedence here is
// total rather than a silent tie-break.
func buildNamespaces(cfg config.Config) (*namespace.Registry, error) {
	if strings.TrimSpace(cfg.Namespaces.File) != "" {
		return namespace.ParseFile(strings.TrimSpace(cfg.Namespaces.File), nil)
	}
	if strings.TrimSpace(cfg.Namespaces.Inline) != "" {
		return namespace.Parse(cfg.Namespaces.Inline, nil)
	}
	return namespace.NewRegistry(nil, nil)
}
