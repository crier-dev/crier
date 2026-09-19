package ci_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/webhook"
	"gopkg.in/yaml.v3"
)

const registrationFixture = "examples/agent-ecosystem/registration-payloads.json"

type registrationPayload struct {
	ID        string          `json:"id"`
	PublicKey string          `json:"public_key"`
	Webhook   json.RawMessage `json:"webhook"`
	Guard     json.RawMessage `json:"guard"`
}

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func loadRegistrationPayloads(t *testing.T) map[string]registrationPayload {
	t.Helper()

	path := repoPath(filepath.FromSlash(registrationFixture))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read real agent-ecosystem registration payloads %s: %v", path, err)
	}
	var payloads map[string]registrationPayload
	if err := json.Unmarshal(raw, &payloads); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	for _, name := range []string{"sink", "consumer", "hermes"} {
		if _, ok := payloads[name]; !ok {
			t.Errorf("%s has no %q registration payload", path, name)
		}
	}
	if len(payloads) != 3 {
		t.Errorf("%s has %d payloads, want exactly sink, consumer, hermes", path, len(payloads))
	}
	return payloads
}

func TestAgentEcosystemRegistrationPayloadsMatchStrictWebhookContract(t *testing.T) {
	for name, payload := range loadRegistrationPayloads(t) {
		t.Run(name, func(t *testing.T) {
			if payload.ID == "" || payload.PublicKey == "" || len(payload.Guard) == 0 {
				t.Fatalf("registration payload is incomplete: %+v", payload)
			}
			cfg, err := webhook.DecodeConfig(payload.Webhook)
			if err != nil {
				t.Fatalf("real example webhook rejected by strict decoder: %v\nwebhook: %s", err, payload.Webhook)
			}
			if cfg == nil {
				t.Fatal("real example webhook decoded to nil")
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("real example webhook failed registration validation: %v", err)
			}
		})
	}
}

func TestAgentEcosystemRegistrationPayloadsRejectUnknownWebhookField(t *testing.T) {
	for name, payload := range loadRegistrationPayloads(t) {
		t.Run(name, func(t *testing.T) {
			var invalid map[string]any
			if err := json.Unmarshal(payload.Webhook, &invalid); err != nil {
				t.Fatalf("decode webhook for negative control: %v", err)
			}
			invalid["response_map"] = "raw"
			raw, err := json.Marshal(invalid)
			if err != nil {
				t.Fatalf("encode negative control: %v", err)
			}

			if cfg, err := webhook.DecodeConfig(raw); err == nil {
				t.Fatalf("strict decoder accepted deliberate unknown webhook-level response_map: %+v", cfg)
			} else if !strings.Contains(err.Error(), `unknown field "response_map"`) {
				t.Fatalf("negative control failed for the wrong reason: %v", err)
			}
		})
	}
}

func TestAgentEcosystemRegistrationFixtureIsUsedByEntrypoints(t *testing.T) {
	for _, rel := range []string{
		"examples/agent-ecosystem/sink/echo_sink.py",
		"examples/agent-ecosystem/consumer/consumer.mjs",
		"examples/agent-ecosystem/hermes/cont-init.d/10-register-crier.sh",
	} {
		raw, err := os.ReadFile(repoPath(filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(raw), "registration-payloads.json") {
			t.Errorf("%s does not load %s", rel, registrationFixture)
		}
	}

	for _, rel := range []string{
		"examples/agent-ecosystem/sink/Dockerfile",
		"examples/agent-ecosystem/pi-agent/Dockerfile",
		"examples/agent-ecosystem/opencode/Dockerfile",
		"examples/agent-ecosystem/claude-code/Dockerfile",
		"examples/agent-ecosystem/codex/Dockerfile",
		"examples/agent-ecosystem/aider/Dockerfile",
		"examples/agent-ecosystem/goose/Dockerfile",
	} {
		raw, err := os.ReadFile(repoPath(filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(raw), "registration-payloads.json") {
			t.Errorf("%s does not package %s", rel, registrationFixture)
		}
	}

	compose, err := os.ReadFile(repoPath("examples", "agent-ecosystem", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read agent-ecosystem compose file: %v", err)
	}
	if !strings.Contains(string(compose), "./registration-payloads.json:/etc/crier/registration-payloads.json:ro") {
		t.Error("Hermes service does not mount the checked registration payload fixture")
	}
}

func TestBunkerWorkflowConcurrencyBoundaries(t *testing.T) {
	raw, err := os.ReadFile(repoPath(".github", "workflows", "bunker-e2e.yml"))
	if err != nil {
		t.Fatalf("read bunker workflow: %v", err)
	}
	var workflow struct {
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress any    `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			If          string `yaml:"if"`
			Concurrency struct {
				Group            string `yaml:"group"`
				CancelInProgress any    `yaml:"cancel-in-progress"`
			} `yaml:"concurrency"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse bunker workflow: %v", err)
	}
	if !strings.Contains(workflow.Concurrency.Group, "github.event_name") {
		t.Errorf("workflow concurrency group = %q, want event-scoped group so pushes cannot share a cancellation domain with schedules", workflow.Concurrency.Group)
	}
	workflowCancel, ok := workflow.Concurrency.CancelInProgress.(string)
	if !ok || !strings.Contains(workflowCancel, "github.event_name == 'push'") {
		t.Errorf("workflow cancel-in-progress = %#v, want push-only cancellation expression", workflow.Concurrency.CancelInProgress)
	}

	matrix := workflow.Jobs["bunker-matrix"]
	battery := workflow.Jobs["ecosystem-battery"]
	if matrix.Concurrency.Group != "bunker-e2e-target" {
		t.Errorf("bunker-matrix concurrency group = %q, want shared bunker target group", matrix.Concurrency.Group)
	}
	cancelExpr, ok := matrix.Concurrency.CancelInProgress.(string)
	if !ok || !strings.Contains(cancelExpr, "github.event_name == 'push'") {
		t.Errorf("bunker-matrix cancel-in-progress = %#v, want push-only cancellation expression", matrix.Concurrency.CancelInProgress)
	}
	if battery.Concurrency.Group != "bunker-e2e-ecosystem" {
		t.Errorf("ecosystem-battery concurrency group = %q, want isolated ecosystem group", battery.Concurrency.Group)
	}
	if cancel, ok := battery.Concurrency.CancelInProgress.(bool); !ok || cancel {
		t.Errorf("ecosystem-battery cancel-in-progress = %#v, want false", battery.Concurrency.CancelInProgress)
	}
	if strings.Contains(battery.If, "push") {
		t.Errorf("ecosystem-battery unexpectedly runs on push: if = %q", battery.If)
	}
	if matrix.Concurrency.Group == battery.Concurrency.Group {
		t.Fatal("push bunker jobs and the scheduled ecosystem battery share a cancellation domain")
	}
}
