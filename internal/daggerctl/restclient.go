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

// RESTClientTimeout bounds one control call made by an out-of-process client.
const RESTClientTimeout = 30 * time.Second

// RESTClient drives crier's OWN dagger control routes. It exists so an
// out-of-process MCP bridge (cmd/crier-mcp) can control a DAG without keeping a
// second copy of the run records: the records live on the crier server, the
// client only addresses them. It is the client-side twin of the server's
// Service, and both satisfy Control, so the MCP tools act on one set of
// records either way.
type RESTClient struct {
	baseURL string
	token   string
	client  *http.Client
}

var _ Control = (*RESTClient)(nil)

// NewRESTClient builds a client for the crier server at baseURL. An empty
// base URL yields a nil client, which the caller treats as "not configured" —
// the MCP layer gates on CRIER_HTTP_URL before it ever reaches here.
func NewRESTClient(baseURL, token string) *RESTClient {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil
	}
	return &RESTClient{
		baseURL: strings.TrimRight(raw, "/"),
		token:   token,
		client:  &http.Client{Timeout: RESTClientTimeout},
	}
}

// CreateRun starts a prompt-driven DAG on the server.
func (c *RESTClient) CreateRun(ctx context.Context, req CreateRunRequest) (*RunRecord, error) {
	return c.call(ctx, http.MethodPost, "/dagger/runs", map[string]any{
		"agent_id": req.AgentID,
		"prompt":   req.Prompt,
	})
}

// RunStatus reads a run's record back from the server.
func (c *RESTClient) RunStatus(ctx context.Context, runID string) (*RunRecord, error) {
	return c.call(ctx, http.MethodGet, "/dagger/runs/"+url.PathEscape(runID), nil)
}

// Cancel cancels a run.
func (c *RESTClient) Cancel(ctx context.Context, runID string) (*RunRecord, error) {
	return c.call(ctx, http.MethodPost, "/dagger/runs/"+url.PathEscape(runID)+"/cancel", nil)
}

// Resume resumes a run from its last checkpoint.
func (c *RESTClient) Resume(ctx context.Context, runID string) (*RunRecord, error) {
	return c.call(ctx, http.MethodPost, "/dagger/runs/"+url.PathEscape(runID)+"/resume", nil)
}

// Rewind rewinds a run to a node.
func (c *RESTClient) Rewind(ctx context.Context, runID, nodeID string) (*RunRecord, error) {
	return c.call(ctx, http.MethodPost, "/dagger/runs/"+url.PathEscape(runID)+"/rewind", map[string]any{
		"node_id": nodeID,
	})
}

// RunSkill runs a registered skill.
func (c *RESTClient) RunSkill(ctx context.Context, req RunSkillRequest) (*RunRecord, error) {
	body := map[string]any{"agent_id": req.AgentID}
	if len(req.Args) > 0 {
		body["args"] = req.Args
	}
	return c.call(ctx, http.MethodPost, "/dagger/skills/"+url.PathEscape(req.Skill)+"/run", body)
}

// call performs one request and decodes the run record. A 404 becomes
// ErrRunNotFound; every other non-2xx becomes ErrBridge carrying the status
// and the server's own error text.
func (c *RESTClient) call(ctx context.Context, method, path string, body any) (*RunRecord, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: encode request: %v", ErrBridge, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrBridge, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
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
	var rec RunRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("%w: decode run record: %v", ErrBridge, err)
	}
	if strings.TrimSpace(rec.RunID) == "" {
		return nil, fmt.Errorf("%w: run record carries no run_id", ErrBridge)
	}
	return &rec, nil
}
