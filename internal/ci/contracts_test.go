package ci_test

import (
	"encoding/json"
	"fmt"
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

// DF-CRIER-291: a tag push must publish the assets. The release workflow is the
// publish half of docs/releases.md §4, so its contract is pinned here the way
// the bunker workflow's concurrency boundaries are pinned above — parse the
// YAML and assert the load-bearing facts: the tag trigger admits only vX.Y.Z
// and vX.Y.Z-rcN, the token grant is least-privilege, the build is pinned to
// the tagged commit with an explicit VERSION, the publish goes through the
// shared scripts/release-upload.sh (never a duplicated `gh release create`,
// never --clobber), and the run ends by diffing the Release's attached assets
// against the SHA256SUMS manifest — the `assets: []` regression stays a loud
// failure instead of a silent one.
func TestReleaseWorkflowContract(t *testing.T) {
	raw, err := os.ReadFile(repoPath(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}

	var wf struct {
		Trigger struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
		Permissions struct {
			Contents string `yaml:"contents"`
		} `yaml:"permissions"`
		Jobs map[string]struct {
			RunsOn any `yaml:"runs-on"`
			Steps  []struct {
				Name string            `yaml:"name"`
				Uses string            `yaml:"uses"`
				With map[string]any    `yaml:"with"`
				Env  map[string]string `yaml:"env"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	if len(wf.Trigger.Push.Tags) != 2 {
		t.Fatalf("release workflow tag patterns = %q, want exactly the stable and rc tag patterns", wf.Trigger.Push.Tags)
	}
	var hasStable, hasRc bool
	for _, pattern := range wf.Trigger.Push.Tags {
		switch pattern {
		case "v[0-9]+.[0-9]+.[0-9]+":
			hasStable = true
		case "v[0-9]+.[0-9]+.[0-9]+-rc[0-9]+":
			hasRc = true
		}
	}
	if !hasStable || !hasRc {
		t.Fatalf("release workflow tag patterns = %q, want v[0-9]+.[0-9]+.[0-9]+ and v[0-9]+.[0-9]+.[0-9]+-rc[0-9]+", wf.Trigger.Push.Tags)
	}
	if wf.Permissions.Contents != "write" {
		t.Fatalf("release workflow permissions.contents = %q, want \"write\" (contents-only least privilege)", wf.Permissions.Contents)
	}
	if strings.Contains(string(raw), "write-all") {
		t.Error("release workflow grants write-all somewhere — least-privilege regression")
	}

	var (
		checkout  bool
		goSetup   bool
		ubuntuRun bool
		ghToken   bool
		runTexts  []string
	)
	for _, job := range wf.Jobs {
		switch v := job.RunsOn.(type) {
		case string:
			if v == "ubuntu-latest" {
				ubuntuRun = true
			}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && s == "ubuntu-latest" {
					ubuntuRun = true
				}
			}
		}
		for _, step := range job.Steps {
			if step.Uses != "" {
				runTexts = append(runTexts, step.Uses)
			}
			if step.Run != "" {
				runTexts = append(runTexts, step.Run)
			}
			if strings.HasPrefix(step.Uses, "actions/checkout@") && fmt.Sprint(step.With["fetch-depth"]) == "0" {
				checkout = true
			}
			if strings.HasPrefix(step.Uses, "actions/setup-go@") && fmt.Sprint(step.With["go-version"]) == "1.26.6" {
				goSetup = true
			}
			if _, ok := step.Env["GH_TOKEN"]; ok {
				ghToken = true
			}
		}
	}
	blob := strings.Join(runTexts, "\n")

	if !checkout {
		t.Error("release workflow has no actions/checkout step with fetch-depth: 0 — the tag and the previous tag must both resolve locally (provenance check, compare link)")
	}
	if !goSetup {
		t.Error("release workflow has no actions/setup-go step pinned to go-version 1.26.6 — the toolchain CI builds and tests with")
	}
	if !ubuntuRun {
		t.Error("release workflow runs on no ubuntu-latest job — the host target (linux/amd64) is a shipped release target and release-artifacts.sh executes the host artifact to prove the identity stamp landed")
	}
	if !ghToken {
		t.Error("no release workflow step carries GH_TOKEN in env — gh cannot authenticate non-interactively inside Actions")
	}

	for _, want := range []struct{ needle, why string }{
		{"refs/tags/${VERSION}^{commit}", "the workflow must prove the checkout IS the tagged commit (no floating source checkout)"},
		{"git rev-parse HEAD", "the identity pin compares the tag's commit against the checked-out HEAD"},
		{"make release-artifacts VERSION=", "the asset set must be built with the tag passed explicitly, never an implicit version"},
		{"scripts/release-upload.sh", "the publish must go through the shared release-upload script — one publish implementation for hand and CI"},
		{"SHA256SUMS", "the run must verify the published assets against the checksum manifest"},
		{"PRERELEASE", "prerelease marking must be wired (rc tags prerelease, stable tags not)"},
	} {
		if !strings.Contains(blob, want.needle) {
			t.Errorf("release workflow steps do not contain %q — %s", want.needle, want.why)
		}
	}
	for _, banned := range []struct{ needle, why string }{
		{"gh release create", "a second publish implementation beside scripts/release-upload.sh would drift from it"},
		{"--clobber", "asset upload must never silently overwrite a published binary"},
	} {
		if strings.Contains(blob, banned.needle) {
			t.Errorf("release workflow steps contain %q — %s", banned.needle, banned.why)
		}
	}
	fields := strings.Fields(blob)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "make" && fields[i+1] == "release" {
			t.Error("release workflow invokes bare `make release` — that target cuts a tag and must never run in CI")
		}
	}
}
