package webhook

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// placeholderRe matches {{path}} / {{path|default:...}} placeholders.
var placeholderRe = regexp.MustCompile(`\{\{[^{}]+\}\}`)

// Template is a named endpoint schema: it maps the Crier envelope onto a
// backend's HTTP request and extracts the reply from its response
// (CR-FEAT-003 — "you supply the schema").
type Template struct {
	Name         string       `json:"name"`
	RequestShape RequestShape `json:"request_shape"`
	ResponseMap  string       `json:"response_map"` // "raw" or dot path e.g. choices.0.message.content
}

// RequestShape describes the outbound HTTP request the schema produces.
type RequestShape struct {
	Method  string            `json:"method,omitempty"` // default POST
	Headers map[string]string `json:"headers,omitempty"`
	// Body is a JSON template with {{path}} placeholders. Paths resolve
	// against TemplateContext. A nil Body means passthrough: the full Crier
	// envelope is POSTed unchanged (generic-custom default).
	Body json.RawMessage `json:"body,omitempty"`
}

// TemplateContext is what {{path}} placeholders resolve against.
type TemplateContext struct {
	Crier   EnvelopeMeta   `json:"crier"`
	Payload map[string]any `json:"payload"`
	Agent   TemplateAgent  `json:"agent"`
	Auth    map[string]string `json:"auth"`
}

// TemplateAgent is the target-agent slice of the context.
type TemplateAgent struct {
	ID           string   `json:"id"`
	Capabilities []string `json:"capabilities"`
	DeliveryMode string   `json:"delivery_mode"`
}

// v1 named templates (spec §6).
var genericCustom = Template{
	Name:         "generic-custom",
	ResponseMap:  "raw",
	RequestShape: RequestShape{Method: "POST"},
}

var openAICompatible = Template{
	Name: "openai-compatible",
	RequestShape: RequestShape{
		Method: "POST",
		Body: json.RawMessage(`{
  "model": "{{agent.model|default:deepseek-v4-flash}}",
  "messages": [{"role": "user", "content": "{{payload.text}}"}],
  "stream": false
}`),
	},
	ResponseMap: "choices.0.message.content",
}

var hermesGateway = Template{
	Name: "hermes-http-gateway",
	RequestShape: RequestShape{
		Method: "POST",
		Body: json.RawMessage(`{
  "model": "{{agent.model|default:deepseek-v4-flash}}",
  "messages": [{"role": "user", "content": "{{payload.text}}"}],
  "stream": false,
  "session_id": "{{crier.session_id}}",
  "thread_id": "{{crier.thread_id}}"
}`),
	},
	ResponseMap: "choices.0.message.content",
}

// templatesByName is the named registry.
var templatesByName = map[string]Template{
	"generic-custom":      genericCustom,
	"openai-compatible":   openAICompatible,
	"hermes-http-gateway": hermesGateway,
}

// ResolveTemplate returns the effective template for a webhook config:
// custom schema wins, else the named template, else generic-custom.
func ResolveTemplate(cfg *Config) Template {
	if cfg.CustomSchema != nil {
		t := genericCustom
		t.ResponseMap = cfg.CustomSchema.ResponseMap
		if cfg.CustomSchema.RequestShape != nil {
			t.RequestShape = *cfg.CustomSchema.RequestShape
		}
		if t.ResponseMap == "" {
			t.ResponseMap = "raw"
		}
		return t
	}
	name := cfg.SchemaTemplate
	if name == "" {
		name = "generic-custom"
	}
	if t, ok := templatesByName[name]; ok {
		return t
	}
	return genericCustom
}

// BuildBody renders the request body for a template: passthrough envelope
// for generic-custom, or the expanded {{path}} template.
func (t Template) BuildBody(cfg *Config, env *Envelope) ([]byte, error) {
	if len(t.RequestShape.Body) == 0 {
		// Passthrough: the full envelope.
		return json.Marshal(env)
	}
	ctx := buildContext(cfg, env)
	expanded, err := expandTemplate(t.RequestShape.Body, ctx)
	if err != nil {
		return nil, err
	}
	// The template may render a JSON object; validate + compact.
	var v any
	if err := json.Unmarshal(expanded, &v); err != nil {
		return nil, fmt.Errorf("template rendered invalid json: %w", err)
	}
	return json.Marshal(v)
}

// ExtractReply pulls the reply out of a webhook response body per the
// template's response map. The returned bytes are ALWAYS valid JSON: "raw"
// passes through JSON bodies and wraps plain text in a JSON string; a dot
// path returns the selected value re-marshaled (strings quoted).
func (t Template) ExtractReply(body []byte) ([]byte, error) {
	if t.ResponseMap == "" || t.ResponseMap == "raw" {
		if json.Valid(body) {
			return body, nil
		}
		return json.Marshal(string(body))
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("reply is not valid json: %w", err)
	}
	cur := v
	for _, seg := range strings.Split(t.ResponseMap, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, fmt.Errorf("response map %q: missing key %q", t.ResponseMap, seg)
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("response map %q: bad array index %q", t.ResponseMap, seg)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("response map %q: cannot descend into %T", t.ResponseMap, cur)
		}
	}
	// Strings re-marshal quoted (valid JSON); everything else re-marshals.
	return json.Marshal(cur)
}

// buildContext assembles the template expansion context from a config+envelope.
func buildContext(cfg *Config, env *Envelope) TemplateContext {
	ctx := TemplateContext{
		Crier: env.Crier,
		Agent: TemplateAgent{ID: env.Crier.Sender, DeliveryMode: env.Crier.DeliveryMode},
		Auth:  map[string]string{},
	}
	_ = json.Unmarshal(env.Payload, &ctx.Payload)
	if ctx.Payload == nil {
		ctx.Payload = map[string]any{}
	}
	return ctx
}

// expandTemplate substitutes {{path[|default:...]}} placeholders in a JSON
// template. Paths resolve against the context; missing values become the
// default or empty string.
func expandTemplate(raw json.RawMessage, ctx TemplateContext) ([]byte, error) {
	s := string(raw)
	var err error
	s = placeholderRe.ReplaceAllStringFunc(s, func(ph string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(ph, "{{"), "}}")
		path := inner
		def := ""
		if i := strings.Index(inner, "|default:"); i >= 0 {
			path = strings.TrimSpace(inner[:i])
			def = inner[i+len("|default:"):]
		}
		val, ok := resolvePath(ctx, strings.TrimSpace(path))
		if !ok {
			return def
		}
		b, e := json.Marshal(val)
		if e != nil {
			err = e
			return def
		}
		// Strings interpolate INSIDE the template's own quotes — emit them
		// JSON-escaped but unquoted ("{{payload.text}}" -> "hello"). Non-strings
		// (numbers, bools, objects) stay JSON-typed.
		if s, isStr := val.(string); isStr {
			if len(b) >= 2 {
				return string(b[1 : len(b)-1])
			}
			return s
		}
		return string(b)
	})
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// resolvePath walks a dotted path into the context. Returns ok=false when
// any segment is missing.
func resolvePath(ctx TemplateContext, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	cur := any(map[string]any{
		"crier":   ctx.Crier,
		"payload": ctx.Payload,
		"agent":   ctx.Agent,
		"auth":    ctx.Auth,
	})
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}
