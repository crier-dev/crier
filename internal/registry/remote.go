package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RemoteStore is a Store implementation that talks to a running Crier server
// over its HTTP API. It lets a client process (e.g. crier-mcp) act as a
// bridge: harness-side tool calls become registry/inbox operations on a
// shared server, so multiple clients see the same agents and inboxes without
// sharing an in-process store or a database connection.
//
// With the server's per-agent signing disabled (CR_REQUIRE_AGENT_SIG=false),
// the only identity this store needs is the agent id sent via X-Agent-ID.
type RemoteStore struct {
	baseURL string
	agentID string
	token   string
	client  *http.Client
}

// NewRemoteStore creates a RemoteStore for the given server base URL.
// agentID is the identity used on agent-owned routes; token (optional) is the
// shared bearer token when the server runs with CR_AUTH_TOKEN.
func NewRemoteStore(baseURL, agentID, token string) *RemoteStore {
	return &RemoteStore{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		agentID: agentID,
		token:   token,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// ---- HTTP plumbing -------------------------------------------------------

func (s *RemoteStore) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", s.agentID)
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("remote %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		if out != nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("decode %s %s response: %w", method, path, err)
			}
		}
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %q", ErrAgentNotFound, s.agentID)
	case http.StatusConflict:
		return ErrAgentExists
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrInvalidStoreInput, string(raw))
	default:
		return fmt.Errorf("remote %s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
	}
}

// ---- Store interface -----------------------------------------------------

func (s *RemoteStore) Register(agent *Agent) error {
	// PublicKey is a HexKey ([]byte with hex MarshalJSON); pass it as the
	// typed value so it serializes as a hex string, not raw bytes.
	return s.do(http.MethodPost, "/agents", map[string]any{
		"id":           agent.ID,
		"public_key":   agent.PublicKey,
		"capabilities": agent.Capabilities,
	}, nil)
}

func (s *RemoteStore) Get(id string) (*Agent, error) {
	var agent Agent
	if err := s.do(http.MethodGet, "/agents/"+url.PathEscape(id), nil, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

func (s *RemoteStore) List() []*Agent {
	var out struct {
		Agents []*Agent `json:"agents"`
	}
	if err := s.do(http.MethodGet, "/agents", nil, &out); err != nil {
		return []*Agent{}
	}
	if out.Agents == nil {
		return []*Agent{}
	}
	return out.Agents
}

func (s *RemoteStore) Unregister(id string) error {
	return s.do(http.MethodDelete, "/agents/"+url.PathEscape(id), nil, nil)
}

func (s *RemoteStore) Deliver(agentID string, entry *InboxEntry) error {
	var out struct {
		ID string `json:"id"`
	}
	if err := s.do(http.MethodPost, "/agents/"+url.PathEscape(agentID)+"/inbox", map[string]any{
		"payload": json.RawMessage(entry.Payload),
	}, &out); err != nil {
		return err
	}
	entry.ID = out.ID // the server assigns the id
	return nil
}

func (s *RemoteStore) Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error) {
	if maxMessages <= 0 {
		maxMessages = 10
	}
	q := url.Values{}
	q.Set("max", strconv.Itoa(maxMessages))
	q.Set("lease", strconv.Itoa(int(leaseDuration.Seconds())))
	var out struct {
		Messages []*InboxEntry `json:"messages"`
		LeaseID  string        `json:"lease_id"`
	}
	if err := s.do(http.MethodGet, "/agents/"+url.PathEscape(agentID)+"/inbox?"+q.Encode(), nil, &out); err != nil {
		return nil, "", err
	}
	if out.Messages == nil {
		out.Messages = []*InboxEntry{}
	}
	for _, e := range out.Messages {
		e.AgentID = agentID
	}
	return out.Messages, out.LeaseID, nil
}

func (s *RemoteStore) Ack(agentID, leaseID string, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return fmt.Errorf("%w: message_ids must not be empty", ErrInvalidStoreInput)
	}
	return s.do(http.MethodPost, "/agents/"+url.PathEscape(agentID)+"/inbox/ack", map[string]any{
		"lease_id":    leaseID,
		"message_ids": messageIDs,
	}, nil)
}

func (s *RemoteStore) Stats(agentID string) (queueDepth, leasedCount int, oldestAge time.Duration, err error) {
	var out struct {
		QueueDepth  int   `json:"queue_depth"`
		LeasedCount int   `json:"leased_count"`
		OldestAgeMs int64 `json:"oldest_age_ms"`
	}
	if err := s.do(http.MethodGet, "/agents/"+url.PathEscape(agentID)+"/inbox/stats", nil, &out); err != nil {
		return 0, 0, 0, err
	}
	return out.QueueDepth, out.LeasedCount, time.Duration(out.OldestAgeMs) * time.Millisecond, nil
}

// PurgeExpired is a no-op: expiry is a server-side concern.
func (s *RemoteStore) PurgeExpired() int { return 0 }
