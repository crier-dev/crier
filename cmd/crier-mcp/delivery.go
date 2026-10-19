package main

import (
	"fmt"
	"strings"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
)

// localDeliveryHandler gives standalone memory/Postgres MCP mode the same
// configured guard and push transport as the HTTP binary. Remote mode uses the
// owning server's HTTP handler instead; never build a second guard there.
func localDeliveryHandler(cfg config.Config, store registry.Store) (*registry.Handler, func(), error) {
	var realms *namespace.Registry
	var err error
	switch {
	case strings.TrimSpace(cfg.Namespaces.File) != "":
		realms, err = namespace.ParseFile(strings.TrimSpace(cfg.Namespaces.File), nil)
	case strings.TrimSpace(cfg.Namespaces.Inline) != "":
		realms, err = namespace.Parse(cfg.Namespaces.Inline, nil)
	default:
		realms, err = namespace.NewRegistry(nil, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	h := registry.NewHandler(store)
	h.SetNamespacePolicies(realms)
	h.SetIdempotencyWindow(cfg.IdempotencyWindow)
	cleanup := func() {}
	if cfg.Guard.Enabled {
		var writer guard.CardWriter = guard.NewHermesKanbanWriter("hermes")
		if cfg.Guard.KanbanURL != "" {
			httpWriter := guard.NewHTTPKanbanWriter(cfg.Guard.KanbanURL)
			if httpWriter == nil {
				return nil, nil, fmt.Errorf("invalid CR_GUARD_KANBAN_URL (http/https scheme required)")
			}
			writer = httpWriter
		}
		filter, err := guard.New(guard.Options{
			Timeout: cfg.Guard.Timeout, MaxConcurrent: cfg.Guard.MaxConcurrent,
			CircuitThreshold: cfg.Guard.CircuitThreshold, CircuitCooldown: cfg.Guard.CircuitCooldown,
			MaxPayloadBytes: cfg.Guard.MaxPayloadBytes, RenderMaxBytes: cfg.Guard.RenderMaxBytes,
			DeepSeekBaseURL: cfg.Guard.DeepSeekBaseURL, DefaultModel: cfg.Guard.Model,
			ExtraPatterns: cfg.Guard.ExtraPatterns, DefaultPolicyJSON: cfg.Guard.DefaultPolicy,
			KanbanWriter: writer, KanbanQueueSize: cfg.Guard.KanbanQueueSize,
		})
		if err != nil {
			return nil, nil, err
		}
		h.SetGuardFilter(filter)
		cleanup = filter.Close
	}
	// Every MCP delivery requests blocking confirmation, so no async/batch
	// workers or failure-notification queue are needed in this process.
	h.SetWebhookDriver(webhook.NewDriver(webhook.NewClient(cfg.Webhook.Timeout, []byte(cfg.Webhook.Secret)), webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries: cfg.Webhook.MaxRetries, CircuitThreshold: cfg.Webhook.CircuitThreshold,
	}))
	return h, cleanup, nil
}
