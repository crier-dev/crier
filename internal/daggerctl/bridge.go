package daggerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBridgeTimeout bounds one call to the dagger surface. It is a client
// timeout only — crier never runs the DAG, so there is nothing here to cancel.
const DefaultBridgeTimeout = 30 * time.Second

// DaggerBridge is crier's ENTIRE execution vocabulary. Everything the control
// surface can do to a DAG is one of these six calls, and each one is a REQUEST
// to a surface crier does not own. That is the boundary rule (D18) expressed
// as a type: there is no Run, no Step, no scheduler and no evaluator here.
type DaggerBridge interface {
	CreateRun(ctx context.Context, req CreateRunRequest) (*RunView, error)
	RunStatus(ctx context.Context, runID string) (*RunView, error)
	Cancel(ctx context.Context, runID string) (*RunView, error)
	Resume(ctx context.Context, runID string) (*RunView, error)
	Rewind(ctx context.Context, runID, nodeID string) (*RunView, error)
	RunSkill(ctx context.Context, req RunSkillRequest) (*RunView, error)
}

// HTTPBridge is the shipped DaggerBridge: a client of the dagger HTTP/JSON
// surface named by CR_DAGGER_URL. It holds no DAG state — every method is one
// request whose answer is parsed into a RunView and handed straight back.
//
// The contract it speaks (documented in README.md, "Dagger control"):
//
//	POST {base}/execute                 {"prompt": "..."}            -> run view
//	GET  {base}/runs/{run_id}                                        -> run view
//	POST {base}/runs/{run_id}/cancel                                 -> run view
//	POST {base}/runs/{run_id}/resume                                 -> run view
//	POST {base}/runs/{run_id}/rewind    {"node_id": "..."}           -> run view
//	POST {base}/skills/{skill}/run      {"args": {...}}              -> run view
//
// A run view is {"run_id": "...", "status": "...", "evidence": [...],
// "nodes": [...]}; the status word is mapped onto RunState by the vocabulary in
// daggerctl.go, and `state` is accepted as an alias for `status`.
type HTTPBridge struct {
	baseURL string
	token   string
	client  *http.Client
}

// HTTPBridgeOption configures an HTTPBridge.
type HTTPBridgeOption func(*HTTPBridge)

// WithHTTPClient replaces the default client (tests use it to shorten timeouts).
func WithHTTPClient(c *http.Client) HTTPBridgeOption {
	return func(b *HTTPBridge) {
		if c != nil {
			b.client = c
		}
	}
}

// WithBearerToken sets the optional bearer token sent to the dagger surface.
// An empty token sends no Authorization header.
func WithBearerToken(token string) HTTPBridgeOption {
	return func(b *HTTPBridge) { b.token = token }
}

// NewHTTPBridge builds a bridge for the given base URL. An empty or
// unparseable URL is refused here rather than at the first call, so a
// misconfigured CR_DAGGER_URL fails loudly at startup.
func NewHTTPBridge(baseURL string, opts ...HTTPBridgeOption) (*HTTPBridge, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, fmt.Errorf("%w: CR_DAGGER_URL is empty", ErrUnconfigured)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%w: CR_DAGGER_URL %q is not an absolute http(s) URL", ErrInvalidInput, raw)
	}
	b := &HTTPBridge{
		baseURL: strings.TrimRight(raw, "/"),
		client:  &http.Client{Timeout: DefaultBridgeTimeout},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// BaseURL returns the configured endpoint, for startup logging.
func (b *HTTPBridge) BaseURL() string { return b.baseURL }

// runViewWire is the JSON shape both this bridge and the REST client accept
// from a run view. `status` and `state` are both accepted: the dagger tools
// speak `status`, and crier's own records say `state`.
type runViewWire struct {
	RunID    string   `json:"run_id"`
	Status   string   `json:"status"`
	State    string   `json:"state"`
	Evidence []string `json:"evidence"`
	Nodes    []Node   `json:"nodes"`
}

// view converts the wire shape into a RunView, refusing a body with no run id:
// a run crier cannot name is a run it cannot hold or deliver.
func (w runViewWire) view() (*RunView, error) {
	if strings.TrimSpace(w.RunID) == "" {
		return nil, fmt.Errorf("%w: run view carries no run_id", ErrBridge)
	}
	word := w.Status
	if strings.TrimSpace(word) == "" {
		word = w.State
	}
	return &RunView{
		RunID:    w.RunID,
		State:    normalizeState(word),
		Evidence: w.Evidence,
		Nodes:    w.Nodes,
	}, nil
}

// CreateRun asks the executor to start a prompt-driven DAG.
func (b *HTTPBridge) CreateRun(ctx context.Context, req CreateRunRequest) (*RunView, error) {
	return b.call(ctx, http.MethodPost, "/execute", map[string]any{"prompt": req.Prompt})
}

// RunStatus asks the executor for a run's current state.
func (b *HTTPBridge) RunStatus(ctx context.Context, runID string) (*RunView, error) {
	return b.call(ctx, http.MethodGet, "/runs/"+url.PathEscape(runID), nil)
}

// Cancel asks the executor to cancel a running DAG.
func (b *HTTPBridge) Cancel(ctx context.Context, runID string) (*RunView, error) {
	return b.call(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/cancel", nil)
}

// Resume asks the executor to continue a paused DAG from its last checkpoint.
func (b *HTTPBridge) Resume(ctx context.Context, runID string) (*RunView, error) {
	return b.call(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/resume", nil)
}

// Rewind asks the executor to discard checkpoints from nodeID onward.
func (b *HTTPBridge) Rewind(ctx context.Context, runID, nodeID string) (*RunView, error) {
	return b.call(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/rewind", map[string]any{"node_id": nodeID})
}

// RunSkill asks the executor to run a registered skill.
func (b *HTTPBridge) RunSkill(ctx context.Context, req RunSkillRequest) (*RunView, error) {
	body := map[string]any{}
	if len(req.Args) > 0 {
		body["args"] = req.Args
	}
	return b.call(ctx, http.MethodPost, "/skills/"+url.PathEscape(req.Skill)+"/run", body)
}

// call performs one request and decodes the run view. A 404 is translated to
// ErrRunNotFound so a caller can answer 404 without string matching; every
// other non-2xx is an ErrBridge carrying the status and, when the body is
// JSON, the executor's own error text.
func (b *HTTPBridge) call(ctx context.Context, method, path string, body any) (*RunView, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: encode request: %v", ErrBridge, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrBridge, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrBridge, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s %s answered 404", ErrRunNotFound, method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%w: %s %s answered %d: %s", ErrBridge, method, path, resp.StatusCode, bridgeErrorDetail(raw))
	}
	if readErr != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrBridge, readErr)
	}
	var wire runViewWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("%w: decode run view: %v", ErrBridge, err)
	}
	return wire.view()
}

// bridgeErrorDetail extracts a human-readable detail from an error body,
// falling back to a bounded raw excerpt. It never returns unbounded text.
func bridgeErrorDetail(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "(empty body)"
	}
	var envelope struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil {
		switch {
		case envelope.Error != "" && envelope.Detail != "":
			return envelope.Error + ": " + envelope.Detail
		case envelope.Error != "":
			return envelope.Error
		case envelope.Detail != "":
			return envelope.Detail
		}
	}
	const max = 200
	text := string(trimmed)
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}

// Compile-time proof that the shipped bridge satisfies the whole vocabulary.
var _ DaggerBridge = (*HTTPBridge)(nil)
