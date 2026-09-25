package registry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
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
// With per-agent signing enabled (the secure default), configure the agent's
// ed25519 private key via WithSigningKey: every request then carries fresh
// X-Agent-Ts / X-Agent-Sig headers signing "METHOD\n<path>\n<unix-seconds>"
// (path excludes the query string) — the exact payload the server verifies.
type RemoteStore struct {
	baseURL string
	agentID string
	token   string
	priv    ed25519.PrivateKey
	client  *http.Client

	// listMu guards listErr, the recorded failure of the LAST List call
	// (nil when it succeeded), exposed via ListError so bridge callers
	// can tell a failing backend from an empty registry (DF-CRIER-199).
	listMu  sync.Mutex
	listErr error
}

// RemoteOption customizes a RemoteStore.
type RemoteOption func(*RemoteStore)

// WithSigningKey enables per-agent request signing with the given ed25519
// private key: each request is stamped with a fresh unix-seconds timestamp in
// X-Agent-Ts and a hex-encoded signature over "METHOD\n<path>\n<ts>" in
// X-Agent-Sig, where <path> is the escaped URL path with the query string
// excluded. The public half of the key must be registered for the agent
// server-side (see the Register public_key field). A nil key keeps the store
// unsigned — correct for servers running CR_REQUIRE_AGENT_SIG=false.
func WithSigningKey(priv ed25519.PrivateKey) RemoteOption {
	return func(s *RemoteStore) { s.priv = priv }
}

// NewRemoteStore creates a RemoteStore for the given server base URL.
// agentID is the identity used on agent-owned routes; token (optional) is the
// shared bearer token when the server runs with CR_AUTH_TOKEN.
//
// Signing is opt-in via RemoteOption (WithSigningKey); the variadic options
// keep the existing three-argument call sites source-compatible.
func NewRemoteStore(baseURL, agentID, token string, opts ...RemoteOption) *RemoteStore {
	s := &RemoteStore{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		agentID: agentID,
		token:   token,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// signRequest stamps the per-agent signature headers on req when a private
// key is configured. The signed payload must match the server's verification
// byte-for-byte: "METHOD\n<path>\n<unix-seconds>". The server signs over
// r.URL.Path — the DECODED path, query string excluded — so the client signs
// req.URL.Path (not EscapedPath): for a path that needs escaping the wire
// form differs (%20 vs space) and only the decoded form verifies.
func (s *RemoteStore) signRequest(req *http.Request) {
	if len(s.priv) == 0 {
		return
	}
	ts := time.Now().Unix()
	tsStr := strconv.FormatInt(ts, 10)
	payload := []byte(req.Method + "\n" + req.URL.Path + "\n" + tsStr)
	sig := ed25519.Sign(s.priv, payload)
	req.Header.Set(HeaderAgentTS, tsStr)
	req.Header.Set(HeaderAgentSig, hex.EncodeToString(sig))
}

// LoadEd25519PrivateKeyFile reads and parses a private key from a PEM file.
// It exists so CLI entrypoints (e.g. crier-mcp with
// CRIER_AGENT_PRIVATE_KEY_FILE) can load the same `openssl genpkey
// -algorithm ED25519` key the README's signing workflow generates.
//
// The file must contain a PKCS#8 ("PRIVATE KEY") PEM block encoding an
// ed25519 key. Unreadable files, missing/outer-wrong PEM blocks, other PKCS#8
// key types (RSA, EC), and other key encodings all fail with an explicit
// error naming the cause. Key material is never included in any error —
// errors reference the path and the structural problem only.
func LoadEd25519PrivateKeyFile(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, fmt.Errorf("private key file path is empty")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key file: %w", err)
	}
	return ParseEd25519PrivateKeyPEM(pemBytes)
}

// ParseEd25519PrivateKeyPEM parses PKCS#8 PEM bytes into an ed25519 private
// key with the same strictness as LoadEd25519PrivateKeyFile.
func ParseEd25519PrivateKeyPEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM data found in private key file")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("private key PEM block type is %q, want PKCS#8 %q (openssl genpkey output)", block.Type, "PRIVATE KEY")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want ed25519 (openssl genpkey -algorithm ED25519)", parsed)
	}
	return priv, nil
}

// ---- HTTP plumbing -------------------------------------------------------

// send performs the request and returns the HTTP status and the raw response
// body. It does not classify the status: endpoints with their own error
// semantics (Ack's 404-vs-409 split) map it themselves.
func (s *RemoteStore) send(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.baseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", s.agentID)
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	s.signRequest(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("remote %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

func (s *RemoteStore) do(method, path string, body any, out any) error {
	status, raw, err := s.send(method, path, body)
	if err != nil {
		return err
	}
	switch status {
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
		return fmt.Errorf("remote %s %s: status %d: %s", method, path, status, string(raw))
	}
}

// doAck is the ack path's error decoder. On this endpoint a 404 means the
// requested message ID does not exist (not "agent not found", which is the
// shared do() mapping) and a 409 means the message exists under a different
// lease — the two failure classes clients must be able to tell apart
// (DF-CRIER-32).
func (s *RemoteStore) doAck(agentID string, body any) error {
	path := "/agents/" + url.PathEscape(agentID) + "/inbox/ack"
	status, raw, err := s.send(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		// A missing agent and a missing message both answer 404; the body
		// distinguishes them, so surface both sentinels' wording and let the
		// caller match on whichever it needs.
		if strings.Contains(string(raw), ErrAgentNotFound.Error()) {
			return fmt.Errorf("%w: %s", ErrAgentNotFound, strings.TrimSpace(string(raw)))
		}
		return fmt.Errorf("%w: %s", ErrMessageNotFound, strings.TrimSpace(string(raw)))
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrLeaseConflict, strings.TrimSpace(string(raw)))
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrInvalidStoreInput, string(raw))
	default:
		return fmt.Errorf("remote %s: status %d: %s", path, status, string(raw))
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

// List fetches all agents from the remote server.
//
// Per the Store contract (the signature Store.List() []*Agent cannot carry
// an error), a failing request still returns a non-nil empty slice — but
// unlike before, the failure is no longer silent: it is logged at Error
// level once per occurrence (status + response body included via the
// wrapped error from do) and recorded for ListError, so the MCP bridge
// surface can tell a failing backend from an empty registry
// (DF-CRIER-199). ListError describes the LAST call: a successful List
// clears it.
func (s *RemoteStore) List() []*Agent {
	var out struct {
		Agents []*Agent `json:"agents"`
	}
	if err := s.do(http.MethodGet, "/agents", nil, &out); err != nil {
		slog.Error("remote list", "error", err, "store_url", s.baseURL)
		s.listMu.Lock()
		s.listErr = err
		s.listMu.Unlock()
		return []*Agent{}
	}
	s.listMu.Lock()
	s.listErr = nil
	s.listMu.Unlock()
	if out.Agents == nil {
		return []*Agent{}
	}
	return out.Agents
}

// ListError returns the failure of the last List call, nil when it
// succeeded. It implements the optional ListErrorReporter Store capability
// (store.go), which callers like the MCP bridge's list_agents handler use
// to answer with an error instead of a laundered empty agent list.
func (s *RemoteStore) ListError() error {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	return s.listErr
}

func (s *RemoteStore) Unregister(id string) error {
	return s.do(http.MethodDelete, "/agents/"+url.PathEscape(id), nil, nil)
}

func (s *RemoteStore) Deliver(agentID string, entry *InboxEntry) error {
	var out struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"payload": json.RawMessage(entry.Payload),
	}
	// Forward a requested lifetime verbatim so a proxied delivery honors
	// ttl_seconds (including 0 = never expires) instead of silently falling
	// back to the downstream relay's default (DF-CRIER-37). Absent when the
	// caller did not request one — the downstream default then applies.
	if entry.TTLSeconds != nil {
		body["ttl_seconds"] = *entry.TTLSeconds
	}
	// Forward the retrieval priority the same way (CR-FEAT-035): a proxy that
	// dropped it would hand the downstream relay a queue that reads in a
	// different order than the caller asked for. Absent at the default, which
	// is what the downstream relay would store anyway.
	if entry.Priority != MinMessagePriority {
		body["priority"] = entry.Priority
	}
	if err := s.do(http.MethodPost, "/agents/"+url.PathEscape(agentID)+"/inbox", body, &out); err != nil {
		return err
	}
	entry.ID = out.ID // the server assigns the id
	return nil
}

// Retrieve fetches a leased batch from the server. The returned lease ID is
// empty exactly when the server claimed no messages (empty inbox, or every
// message already leased) — in that case messages is a non-nil empty slice and
// there is nothing to ack (DF-CRIER-32).
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

// Ack acknowledges messages on the server. A message ID the server does not
// hold decodes to ErrMessageNotFound (404) and one leased under another lease
// to ErrLeaseConflict (409) so callers can tell the two apart (DF-CRIER-32).
func (s *RemoteStore) Ack(agentID, leaseID string, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return fmt.Errorf("%w: message_ids must not be empty", ErrInvalidStoreInput)
	}
	return s.doAck(agentID, map[string]any{
		"lease_id":    leaseID,
		"message_ids": messageIDs,
	})
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
