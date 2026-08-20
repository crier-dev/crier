// Package webhook implements push-based HTTP delivery for Crier agents:
// per-agent webhook endpoints, outbound envelope POSTs with HMAC signing,
// bounded retries with exponential backoff, a durable queue for offline
// endpoints, and a circuit breaker that degrades poisoned endpoints.
//
// Spec: specs/WEBHOOK-DELIVERY.md (CR-SPEC-001). Ticket: CR-FEAT-001.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// AuthType enumerates supported outbound authentication schemes.
type AuthType string

const (
	AuthNone   AuthType = "none"
	AuthBearer AuthType = "bearer"
)

// BatchConfig controls batch-mode coalescing (CR-FEAT-005).
type BatchConfig struct {
	MaxMessages    int `json:"max_messages,omitempty"`
	FlushIntervalS int `json:"flush_interval_s,omitempty"`
}

// Config is the per-agent webhook configuration carried on registration.
type Config struct {
	URL            string        `json:"url"`
	AuthType       AuthType      `json:"auth_type,omitempty"`
	AuthValueRef   string        `json:"auth_value_ref,omitempty"`
	SchemaTemplate string        `json:"schema_template,omitempty"`
	CustomSchema   *CustomSchema `json:"custom_schema,omitempty"`
	DeliveryMode   string        `json:"delivery_mode,omitempty"` // blocking|async|batch
	Batch          *BatchConfig  `json:"batch,omitempty"`
	Retries        int           `json:"retries,omitempty"`
	TimeoutMs      int           `json:"timeout_ms,omitempty"`
}

// CustomSchema is a bring-your-own endpoint schema (CR-FEAT-003). It wins
// over the named template when present.
type CustomSchema struct {
	// RequestShape overrides the request body/headers template. Nil keeps
	// passthrough (the full Crier envelope is POSTed).
	RequestShape *RequestShape `json:"request_shape,omitempty"`
	// ResponseMap extracts the reply from the response body: "raw" or a dot
	// path (e.g. "choices.0.message.content").
	ResponseMap string `json:"response_map,omitempty"`
}

// Validate checks a webhook config for registration-time errors.
func (c *Config) Validate() error {
	if c.URL == "" {
		return fmt.Errorf("webhook.url is required")
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("webhook.url must be http(s)://")
	}
	switch c.AuthType {
	case "", AuthNone, AuthBearer:
	default:
		return fmt.Errorf("webhook.auth_type must be none|bearer")
	}
	if c.AuthType == AuthBearer && c.AuthValueRef == "" {
		return fmt.Errorf("webhook.auth_value_ref is required for bearer auth")
	}
	if c.DeliveryMode != "" && c.DeliveryMode != "blocking" && c.DeliveryMode != "async" && c.DeliveryMode != "batch" {
		return fmt.Errorf("webhook.delivery_mode must be blocking|async|batch")
	}
	if c.Batch != nil {
		// 0 = unset (driver default applies); negative is a config error.
		if c.Batch.MaxMessages < 0 {
			return fmt.Errorf("webhook.batch.max_messages must be >= 0")
		}
		if c.Batch.FlushIntervalS < 0 {
			return fmt.Errorf("webhook.batch.flush_interval_s must be >= 0")
		}
	}
	if c.Retries < 0 || c.Retries > 10 {
		return fmt.Errorf("webhook.retries must be 0..10")
	}
	if c.TimeoutMs < 0 || c.TimeoutMs > 120000 {
		return fmt.Errorf("webhook.timeout_ms must be 0..120000")
	}
	return nil
}

// Envelope is the outbound webhook body (spec §3).
type Envelope struct {
	Crier   EnvelopeMeta    `json:"crier"`
	Payload json.RawMessage `json:"payload"`
}

// EnvelopeMeta carries the Crier-level fields the endpoint needs.
type EnvelopeMeta struct {
	Version      int    `json:"version"`
	MessageID    string `json:"message_id"`
	RequestID    string `json:"request_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	ThreadID     string `json:"thread_id,omitempty"`
	DeliveryMode string `json:"delivery_mode,omitempty"`
	Sender       string `json:"sender,omitempty"`
	Kind         string `json:"kind,omitempty"`
}

// Client performs webhook POSTs with timeout and optional HMAC signing.
type Client struct {
	http    *http.Client
	secret  []byte // HMAC signing key (CR_WEBHOOK_SECRET); nil = unsigned
	timeout time.Duration
}

// NewClient builds a webhook client. secret may be nil (unsigned).
func NewClient(timeout time.Duration, secret []byte) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		secret:  secret,
		timeout: timeout,
	}
}

// Result describes one delivery attempt outcome.
type Result struct {
	StatusCode int
	Retryable  bool
	Body       []byte // response body (blocking-mode reply source)
	Err        error
}

// Post sends one envelope to the endpoint through its schema template.
// Returns the Result; never panics.
func (c *Client) Post(cfg *Config, env *Envelope, retry int) Result {
	tpl := ResolveTemplate(cfg)
	body, err := tpl.BuildBody(cfg, env)
	if err != nil {
		return Result{Err: fmt.Errorf("build body: %w", err)}
	}
	return c.postBody(cfg, body, env.Crier.Kind, env.Crier.Sender, env.Crier.SessionID, retry)
}

// batchEnvelopeBody is the spec §4 batch payload: {"messages": [envelope, …]}.
type batchEnvelopeBody struct {
	Messages []*Envelope `json:"messages"`
}

// PostBatch coalesces several envelopes into ONE webhook POST with
// X-Crier-Event: batch (spec §4 — CR-FEAT-005). The batch wrapper is always
// the raw {"messages":[...]} body: schema templates shape individual
// messages, not the batch envelope.
func (c *Client) PostBatch(cfg *Config, envs []*Envelope, retry int) Result {
	if len(envs) == 0 {
		return Result{Err: fmt.Errorf("batch: no envelopes")}
	}
	body, err := json.Marshal(batchEnvelopeBody{Messages: envs})
	if err != nil {
		return Result{Err: fmt.Errorf("build batch body: %w", err)}
	}
	sender, session := "", ""
	if envs[0] != nil {
		sender = envs[0].Crier.Sender
		session = envs[0].Crier.SessionID
	}
	return c.postBody(cfg, body, "batch", sender, session, retry)
}

// postBody performs the POST with the webhook contract headers (spec §3)
// and classifies the response.
func (c *Client) postBody(cfg *Config, body []byte, event, sender, session string, retry int) Result {
	req, err := http.NewRequest(http.MethodPost, cfg.URL, strings.NewReader(string(body)))
	if err != nil {
		return Result{Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Crier-Event", event)
	req.Header.Set("X-Crier-Agent", sender)
	req.Header.Set("X-Crier-Retry", fmt.Sprintf("%d", retry))
	if session != "" {
		req.Header.Set("X-Crier-Session", session)
	}
	for k, v := range ResolveTemplate(cfg).RequestShape.Headers {
		req.Header.Set(k, v)
	}
	if c.secret != nil {
		mac := hmac.New(sha256.New, c.secret)
		mac.Write(body)
		req.Header.Set("X-Crier-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	if cfg.AuthType == AuthBearer {
		if tok := bearerToken(cfg.AuthValueRef); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("post %s: %w", cfg.URL, err), Retryable: true}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Result{StatusCode: resp.StatusCode, Body: respBody}
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return Result{StatusCode: resp.StatusCode, Retryable: true}
	case resp.StatusCode >= 500:
		return Result{StatusCode: resp.StatusCode, Retryable: true}
	default:
		return Result{StatusCode: resp.StatusCode}
	}
}

// ExtractReply pulls the reply out of a response body per the config's
// schema template (CR-FEAT-002 blocking mode).
func (c *Client) ExtractReply(cfg *Config, body []byte) ([]byte, error) {
	return ResolveTemplate(cfg).ExtractReply(body)
}

// bearerToken resolves a value_ref of the form "env:VAR" from the process env.
var bearerToken = func(ref string) string {
	if !strings.HasPrefix(ref, "env:") {
		return ""
	}
	return strings.TrimSpace(lookupEnv(strings.TrimPrefix(ref, "env:")))
}

// lookupEnv is indirection for tests.
var lookupEnv = func(k string) string { return os.Getenv(k) }

// backoff returns the retry delay for attempt n (1s, 2s, 4s, 8s, 16s … capped).
func backoff(n int) time.Duration {
	if n <= 0 {
		n = 1
	}
	d := time.Second << uint(min(n-1, 4))
	return d
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// logf is the package logger hook (slog default).
var logf = func(msg string, args ...any) {
	slog.Info(msg, args...)
}
