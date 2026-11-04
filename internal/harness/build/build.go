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
	"github.com/crier-dev/crier/internal/harness/codex"
	"github.com/crier-dev/crier/internal/harness/hermes"
	"github.com/crier-dev/crier/internal/harness/opencode"
	"github.com/crier-dev/crier/internal/harness/pi"
)

// Registry builds the adapter registry from the harness config. A disabled
// adapter block is skipped; every other adapter is unaffected, and a warning is
// logged for each (§4.1).
//
// All four adapters are real: a block that is present is registered, a block
// that was refused (malformed) is absent and only that adapter is missing.
func Registry(cfg harness.Config) *harness.Registry {
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "adapter disabled") {
			slog.Warn("harness adapter disabled", "reason", warning)
			continue
		}
		slog.Warn("harness config", "reason", warning)
	}
	r := harness.NewRegistry(harness.RegistryOptions{
		Enabled:       cfg.Enabled,
		Default:       cfg.Default,
		Timeout:       cfg.Timeout,
		MaxConcurrent: cfg.MaxConcurrent,
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
	if ac, ok := cfg.Adapters[harness.AdapterPi]; ok {
		r.Register(pi.New(pi.Config{
			Binary:     ac.Binary,
			Workdir:    ac.Workdir,
			Model:      ac.Model,
			SessionDir: ac.SessionDir,
			Timeout:    cfg.Timeout,
		}))
	}
	if ac, ok := cfg.Adapters[harness.AdapterCodex]; ok {
		r.Register(codex.New(codex.Config{
			Binary:  ac.Binary,
			Workdir: ac.Workdir,
			Model:   ac.Model,
			Sandbox: ac.Sandbox,
			Timeout: cfg.Timeout,
		}))
	}
	return r
}
