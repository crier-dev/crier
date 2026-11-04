// Package build wires the configured harness adapters into a registry.
//
// It exists because the adapters import package harness: a builder inside
// harness would close an import cycle. Everything that needs a served
// registry (the CLI, and COURIER-INGRESS later) calls Registry.
package build

import (
	"log/slog"
	"strings"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/hermes"
	"github.com/crier-dev/crier/internal/harness/opencode"
)

// UnimplementedReason is what pi and codex report until their rows land: their
// config blocks are accepted, and the capability table says why they are not
// available instead of pretending they do not exist.
const (
	PiReason    = "adapter not implemented in this build (CRIER-HARNESS-2)"
	CodexReason = "adapter not implemented in this build (CRIER-HARNESS-3)"
)

// Registry builds the adapter registry from the harness config. A disabled
// adapter block is skipped; every other adapter is unaffected, and a warning is
// logged for each (§4.1).
func Registry(cfg harness.Config) *harness.Registry {
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "adapter disabled") {
			slog.Warn("harness adapter disabled", "reason", warning)
			continue
		}
		slog.Warn("harness config", "reason", warning)
	}
	known := map[string]string{}
	if _, disabled := cfg.Disabled[harness.AdapterPi]; !disabled {
		known[harness.AdapterPi] = PiReason
	}
	if _, disabled := cfg.Disabled[harness.AdapterCodex]; !disabled {
		known[harness.AdapterCodex] = CodexReason
	}
	r := harness.NewRegistry(harness.RegistryOptions{
		Enabled:          cfg.Enabled,
		Default:          cfg.Default,
		Timeout:          cfg.Timeout,
		MaxConcurrent:    cfg.MaxConcurrent,
		KnownUnavailable: known,
	})
	if ac, ok := cfg.Adapters[harness.AdapterHermes]; ok {
		r.Register(hermes.New(hermes.Config{
			Binary:       ac.Binary,
			Workdir:      ac.Workdir,
			Model:        ac.Model,
			GatewayURL:   ac.GatewayURL,
			GatewayToken: ac.GatewayToken,
			Timeout:      cfg.Timeout,
		}))
	}
	if ac, ok := cfg.Adapters[harness.AdapterOpencode]; ok {
		r.Register(opencode.New(opencode.Config{
			Binary:       ac.Binary,
			Workdir:      ac.Workdir,
			ServerURL:    ac.ServerURL,
			Username:     ac.Username,
			PasswordFile: ac.PasswordFile,
			Timeout:      cfg.Timeout,
		}))
	}
	return r
}
