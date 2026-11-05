// Package hermesession delivers a Crier message into a Hermes gateway SESSION
// instead of an inbox or a webhook endpoint.
//
// The gap it closes: crier could hand a message to a durable inbox or push it
// at a webhook, but it had no way to START a Hermes session or WRITE INTO an
// existing one. Hermes' gateway already exposes exactly that over HTTP —
// POST /api/sessions (create), GET /api/sessions/{id} (inspect),
// POST /api/sessions/{id}/chat (one agent turn) — so this package is a
// delivery TRANSPORT over those three calls, not a new protocol.
//
// What it does, per delivery, in order:
//
//  1. resolve the session the agent's policy asks for (new / existing /
//     per-agent, see Config.Policy);
//  2. if the policy names an existing session, GET it — a missing session and
//     an ENDED session are two different, named failures;
//  3. POST /api/sessions/{id}/chat with the message text;
//  4. record the pairing (crier message id -> Hermes session id + the
//     correlation id the gateway handed back) in a bounded, process-scoped
//     replay window, so the same message id delivered twice answers as a
//     REPLAY and creates no second session.
//
// Credentials are read from a file, never from a flag or a config value, and
// never logged: the token file must be a regular file with no group/other
// permission bits, and every error this package returns names the file's PATH
// and its mode, never its contents.
//
// Spec: specs/HERMES-SESSION-DELIVERY.md.
package hermesession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Transport is the name this delivery path reports on every accept. It is the
// third member of the closed transport vocabulary alongside "inbox" and
// "webhook" (registry.metrics_deliver.go).
const Transport = "hermes-session"

const (
	// DefaultTimeoutMs bounds one gateway call. It matches the deliver API's
	// documented blocking budget (defaultDeliverTimeoutMs = 30000): a session
	// delivery is request/response, so a caller's patience is the same.
	DefaultTimeoutMs = 30000
	// DefaultReplayWindow is how long a delivered message id answers as a
	// replay instead of creating a second session.
	DefaultReplayWindow = 24 * time.Hour
	// maxTokenBytes bounds the credential file. A token is ~40-80 bytes; a
	// megabyte file is a mistake, and reading it unbounded would be one.
	maxTokenBytes = 8 << 10
	// maxReplyBytes bounds what this package retains from a chat response.
	maxReplyBytes = 1 << 20
)

// Policy is a session policy: who owns the session a delivery writes into.
type Policy string

const (
	// PolicyNew starts a session per message.
	PolicyNew Policy = "new"
	// PolicyExisting appends to the configured session id.
	PolicyExisting Policy = "existing"
	// PolicyPerAgent keeps one session per agent, created on first use and
	// recorded in this process.
	PolicyPerAgent Policy = "per-agent"
)

// Config is the per-agent hermes_session configuration carried on registration
// (the hermes_session object of POST /agents and PATCH /agents/{id}).
type Config struct {
	// BaseURL is the gateway's origin, e.g. http://127.0.0.1:8080. The
	// session routes are appended; a trailing slash is tolerated.
	BaseURL string `json:"base_url"`
	// TokenFile is the PATH of the file holding the gateway bearer token.
	// The path is configuration; the token is read at delivery time, so a
	// rotated credential needs no re-registration. Group/world-readable
	// files are refused rather than used.
	TokenFile string `json:"token_file"`
	// Policy is new | existing | per-agent. Empty means new.
	Policy Policy `json:"policy"`
	// SessionID is the session an "existing" policy appends to. Required for
	// that policy and meaningless for the other two.
	SessionID string `json:"session_id,omitempty"`
	// Title names a session this delivery CREATES (new, and the first
	// delivery of per-agent). Blank lets the gateway mint "api_<ts>_<hex>".
	Title string `json:"title,omitempty"`
	// TimeoutMs bounds each gateway call. <=0 means DefaultTimeoutMs.
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// Validate checks the SHAPE of a hermes_session config at registration time.
// It deliberately does not touch the filesystem: a relay that registers an
// agent need not be able to read the credential, and a registration is not a
// delivery. A missing or unsafe token file surfaces on the first delivery as
// HERMES_CREDENTIAL_UNAVAILABLE.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("hermes_session is nil")
	}
	if c.BaseURL == "" {
		return fmt.Errorf("hermes_session.base_url is required")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("hermes_session.base_url must be http(s)://")
	}
	if c.TokenFile == "" {
		return fmt.Errorf("hermes_session.token_file is required (a path; the token is never carried in config)")
	}
	switch c.Policy {
	case "", PolicyNew:
	case PolicyExisting:
		if c.SessionID == "" {
			return fmt.Errorf("hermes_session.session_id is required when policy is %q", PolicyExisting)
		}
	case PolicyPerAgent:
		if c.SessionID != "" {
			return fmt.Errorf("hermes_session.session_id is set by the relay under policy %q; configure it empty", PolicyPerAgent)
		}
	default:
		return fmt.Errorf("hermes_session.policy must be new|existing|per-agent")
	}
	if c.TimeoutMs < 0 {
		return fmt.Errorf("hermes_session.timeout_ms must not be negative")
	}
	return nil
}

// effectivePolicy is the policy a delivery runs under, defaulting to new.
func (c *Config) effectivePolicy() Policy {
	if c.Policy == "" {
		return PolicyNew
	}
	return c.Policy
}

// Request is one message handed to a session.
type Request struct {
	// AgentID is the registered crier agent the delivery is for; it is the
	// key of the per-agent session map and travels as X-Crier-Target.
	AgentID string
	// MessageID is the crier message id. It is the replay key AND the
	// correlation id handed to the gateway (X-Crier-Message-Id), so a reply
	// that comes back quoting it can be matched to the message that caused
	// the turn.
	MessageID string
	// Sender is the originating agent id, travelled as X-Crier-Agent.
	Sender string
	// CrierSessionID is the crier chat session the message belongs to, if
	// any. It is recorded in the ledger and NOT sent to the gateway: crier
	// session ids and Hermes session ids are different namespaces.
	CrierSessionID string
	// Kind is the envelope kind (message, reply, ...), travelled as
	// X-Crier-Event so a Hermes-side handler can tell them apart.
	Kind string
	// Body is the message payload as crier carries it. A JSON string sends
	// its decoded value; anything else sends its JSON text.
	Body []byte
}

// Result is one successful delivery.
type Result struct {
	// SessionID is the Hermes session the turn ran in.
	SessionID string
	// CorrelationID is what the gateway handed back for this turn: the
	// session id in the chat response body, else the X-Hermes-Session-Id
	// header, else the session this delivery addressed. It is what a later
	// reply is matched against.
	CorrelationID string
	// Reply is the assistant content of the turn.
	Reply string
	// Policy is the policy the delivery ran under.
	Policy Policy
	// Replayed is true when the message id had already been delivered and
	// nothing was sent to the gateway this time.
	Replayed bool
}

// Record is what the replay window keeps for one delivered message id.
type Record struct {
	MessageID     string
	SessionID     string
	CorrelationID string
	Reply         string
	Policy        Policy
	Delivered     bool
	At            time.Time
}

// Named failure codes. Each is a distinct condition with its own status: the
// four required cases are the first four; the last two are the credential and
// generic-rejection cases that exist so no gateway answer is ever flattened
// into one of the four.
const (
	// CodeGatewayUnreachable: the gateway could not be reached at all
	// (dial, DNS, TLS, timeout) — nothing was delivered.
	CodeGatewayUnreachable = "HERMES_GATEWAY_UNREACHABLE"
	// CodeGatewayUnauthorized: the gateway answered 401 or 403. Verbatim
	// gateway status in Failure.GatewayStatus.
	CodeGatewayUnauthorized = "HERMES_GATEWAY_UNAUTHORIZED"
	// CodeSessionNotFound: the addressed session does not exist (the
	// gateway's 404 for GET or chat).
	CodeSessionNotFound = "HERMES_SESSION_NOT_FOUND"
	// CodeSessionClosed: the session exists but has ENDED (ended_at or
	// end_reason set). The gateway itself would still run a turn in it; this
	// transport refuses rather than writing into a closed transcript.
	CodeSessionClosed = "HERMES_SESSION_CLOSED"
	// CodeCredentialUnavailable: the token file is missing, unreadable,
	// empty, oversized, or has group/other permission bits.
	CodeCredentialUnavailable = "HERMES_CREDENTIAL_UNAVAILABLE"
	// CodeGatewayRejected: the gateway answered something else entirely
	// (5xx, 4xx other than 401/403/404, a 3xx this client refuses to follow,
	// or a 2xx whose body carries no session id).
	CodeGatewayRejected = "HERMES_GATEWAY_REJECTED"
)

// statuses is the crier-side HTTP status each code answers with. It is a
// closed mapping (never derived from the gateway's status, which is carried
// verbatim alongside), so a sender's error handling does not change when the
// gateway's does.
var statuses = map[string]int{
	CodeGatewayUnreachable:    http.StatusBadGateway, // 502
	CodeGatewayUnauthorized:   http.StatusBadGateway, // 502 — OUR credential, not the sender's
	CodeSessionNotFound:       http.StatusNotFound,   // 404
	CodeSessionClosed:         http.StatusConflict,   // 409
	CodeCredentialUnavailable: http.StatusBadGateway, // 502
	CodeGatewayRejected:       http.StatusBadGateway, // 502
}

// Failure is a NAMED transport failure. Status is crier's answer to the
// sender; GatewayStatus is the gateway's own status, verbatim, or 0 when no
// HTTP exchange happened.
type Failure struct {
	Code          string
	Status        int
	GatewayStatus int
	Detail        string
}

func (f *Failure) Error() string {
	if f.GatewayStatus > 0 {
		return fmt.Sprintf("%s: %s (gateway status %d)", f.Code, f.Detail, f.GatewayStatus)
	}
	return fmt.Sprintf("%s: %s", f.Code, f.Detail)
}

// fail builds a Failure with the code's closed crier status.
func fail(code, detail string, gatewayStatus int) *Failure {
	return &Failure{Code: code, Status: statuses[code], GatewayStatus: gatewayStatus, Detail: detail}
}

// Client performs session deliveries. It is safe for concurrent use and holds
// only the replay window and the per-agent session map — no per-delivery
// state — so one client serves every agent.
type Client struct {
	http *http.Client
	now  func() time.Time

	mu            sync.Mutex
	replayWindow  time.Duration
	records       map[string]*Record
	agentSessions map[string]string
	lastPrune     time.Time

	locks keyedLocks
}

// NewClient builds a session client. A non-positive timeout means
// DefaultTimeoutMs. Redirects are NOT followed: the Authorization header must
// not be replayed at a host the credential was never handed to, so a 3xx is a
// refusal (HERMES_GATEWAY_REJECTED) rather than a hop.
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeoutMs * time.Millisecond
	}
	return &Client{
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:           time.Now,
		replayWindow:  DefaultReplayWindow,
		records:       make(map[string]*Record),
		agentSessions: make(map[string]string),
	}
}

// SetReplayWindow changes how long a message id answers as a replay. A
// non-positive window restores the documented default.
func (c *Client) SetReplayWindow(window time.Duration) {
	if window <= 0 {
		window = DefaultReplayWindow
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replayWindow = window
}

// SetClock replaces the clock the replay window ages against (tests).
func (c *Client) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// Lookup reports what the replay window remembers for a crier message id. It
// is how an operator (or a later reply handler) resolves a message id to the
// Hermes session and correlation id it was delivered under.
func (c *Client) Lookup(messageID string) (Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[messageID]
	if !ok || !rec.Delivered || c.expiredLocked(rec) {
		return Record{}, false
	}
	return *rec, true
}

func (c *Client) expiredLocked(rec *Record) bool {
	return c.now().Sub(rec.At) > c.replayWindow
}

// pruneLocked drops aged records at most once a minute, so a busy relay does
// not walk the whole window on every delivery.
func (c *Client) pruneLocked() {
	now := c.now()
	if now.Sub(c.lastPrune) < time.Minute {
		return
	}
	c.lastPrune = now
	for id, rec := range c.records {
		if now.Sub(rec.At) > c.replayWindow {
			delete(c.records, id)
		}
	}
}

// Deliver hands one message to the session its config names.
func (c *Client) Deliver(ctx context.Context, cfg *Config, req Request) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, fail(CodeGatewayRejected, err.Error(), 0)
	}
	if strings.TrimSpace(req.MessageID) == "" {
		return Result{}, fail(CodeGatewayRejected, "delivery carries no message id; a session delivery is replayed by message id and cannot run without one", 0)
	}
	policy := cfg.effectivePolicy()
	// One message id at a time: two concurrent deliveries of the same id must
	// not both create a session. The lock is per id, so unrelated messages
	// never queue behind each other.
	unlock := c.locks.lock("message:" + req.MessageID)
	defer unlock()

	if rec, ok := c.lookupDelivered(req.MessageID); ok {
		return Result{SessionID: rec.SessionID, CorrelationID: rec.CorrelationID,
			Reply: rec.Reply, Policy: rec.Policy, Replayed: true}, nil
	}

	token, err := readToken(cfg.TokenFile)
	if err != nil {
		return Result{}, fail(CodeCredentialUnavailable, err.Error(), 0)
	}

	// A previous attempt under this message id may have CREATED its session
	// and then failed the chat. Reuse that session: creating a second one is
	// exactly the duplicate the replay window exists to prevent.
	sessionID, _, err := c.sessionFor(ctx, cfg, policy, req, token)
	if err != nil {
		return Result{}, err
	}

	correlationID, reply, err := c.chat(ctx, cfg, token, sessionID, req)
	if err != nil {
		// Record the session so a retry writes into it rather than minting
		// another one, and keep Delivered false so the retry is not answered
		// as a replay of a turn that never ran.
		c.record(req.MessageID, &Record{MessageID: req.MessageID, SessionID: sessionID,
			Policy: policy, Delivered: false, At: c.now()})
		return Result{}, err
	}
	if correlationID == "" {
		correlationID = sessionID
	}

	c.record(req.MessageID, &Record{MessageID: req.MessageID, SessionID: sessionID,
		CorrelationID: correlationID, Reply: reply, Policy: policy, Delivered: true, At: c.now()})
	if policy == PolicyPerAgent {
		c.mu.Lock()
		c.agentSessions[req.AgentID] = sessionID
		c.mu.Unlock()
	}
	return Result{SessionID: sessionID, CorrelationID: correlationID, Reply: reply, Policy: policy}, nil
}

// lookupDelivered returns the record for a message id when it was delivered
// inside the replay window.
func (c *Client) lookupDelivered(messageID string) (Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[messageID]
	if !ok || !rec.Delivered || c.expiredLocked(rec) {
		return Record{}, false
	}
	return *rec, true
}

// pendingSession returns the session an earlier attempt recorded for this
// message id without completing its turn.
func (c *Client) pendingSession(messageID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.records[messageID]
	if rec == nil || rec.Delivered {
		return ""
	}
	return rec.SessionID
}

func (c *Client) record(messageID string, rec *Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	c.records[messageID] = rec
}

// sessionFor resolves (or creates) the session this delivery addresses. The
// bool reports whether the session already existed before this call.
func (c *Client) sessionFor(ctx context.Context, cfg *Config, policy Policy, req Request, token string) (string, bool, error) {
	// A session this message id already created but never got a turn into is
	// reused, whatever the creation policy was. Only 404 = "that session is
	// gone, mint another"; any other refusal is surfaced.
	if pending := c.pendingSession(req.MessageID); pending != "" {
		if err := c.requireOpen(ctx, cfg, token, pending); err == nil {
			return pending, true, nil
		} else {
			var f *Failure
			if !errors.As(err, &f) || f.Code != CodeSessionNotFound {
				return "", true, err
			}
		}
	}
	switch policy {
	case PolicyExisting:
		if err := c.requireOpen(ctx, cfg, token, cfg.SessionID); err != nil {
			return "", true, err
		}
		return cfg.SessionID, true, nil
	case PolicyPerAgent:
		if sessionID, err := c.agentSession(req.AgentID); err == nil {
			if err := c.requireOpen(ctx, cfg, token, sessionID); err != nil {
				var f *Failure
				if errors.As(err, &f) && f.Code == CodeSessionNotFound {
					// The recorded session is gone. Start a fresh one rather
					// than failing every later delivery for this agent.
					break
				}
				return "", true, err
			}
			return sessionID, true, nil
		}
	}

	// policy new, or per-agent with no live session: create one.
	title := cfg.Title
	if title == "" && policy == PolicyPerAgent {
		title = "crier: " + req.AgentID
	}
	sessionID, err := c.createSession(ctx, cfg, token, title)
	if err != nil {
		return "", false, err
	}
	if policy == PolicyPerAgent {
		c.mu.Lock()
		c.agentSessions[req.AgentID] = sessionID
		c.mu.Unlock()
	}
	return sessionID, false, nil
}

// agentSession returns the recorded per-agent session, if any.
func (c *Client) agentSession(agentID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sessionID := c.agentSessions[agentID]
	if sessionID == "" {
		return "", fmt.Errorf("no session recorded for agent %s", agentID)
	}
	return sessionID, nil
}

// gatewaySession is the subset of a gateway session row this transport reads.
// ended_at arrives as a unix number, so it is kept RAW: a schema that starts
// sending a string must not make every delivery fail to decode.
type gatewaySession struct {
	ID        string          `json:"id"`
	EndReason string          `json:"end_reason"`
	EndedAt   json.RawMessage `json:"ended_at"`
}

func (s gatewaySession) closed() bool {
	if strings.TrimSpace(s.EndReason) != "" {
		return true
	}
	raw := strings.TrimSpace(string(s.EndedAt))
	return raw != "" && raw != "null" && raw != "0" && raw != `""`
}

// requireOpen proves the session exists and has not ended, mapping the three
// possible answers onto the three distinct failures.
func (c *Client) requireOpen(ctx context.Context, cfg *Config, token, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fail(CodeSessionNotFound, "no session id was configured", 0)
	}
	status, body, _, err := c.do(ctx, cfg, token, http.MethodGet, "/api/sessions/"+sessionID, nil, Request{})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound:
		return fail(CodeSessionNotFound, fmt.Sprintf("session %q does not exist", sessionID), status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fail(CodeGatewayUnauthorized, "the gateway rejected the credential crier was configured with", status)
	case status < 200 || status > 299:
		return fail(CodeGatewayRejected, fmt.Sprintf("gateway answered %d reading session %q: %s", status, sessionID, snippet(body)), status)
	}
	var out struct {
		Session gatewaySession `json:"session"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Session.closed() {
		return fail(CodeSessionClosed, fmt.Sprintf("session %q has ended", sessionID), status)
	}
	return nil
}

// createSession starts one empty Hermes session and returns its id.
func (c *Client) createSession(ctx context.Context, cfg *Config, token, title string) (string, error) {
	body := map[string]any{"source": "crier"}
	if strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	status, raw, header, err := c.do(ctx, cfg, token, http.MethodPost, "/api/sessions", body, Request{})
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "", fail(CodeGatewayUnauthorized, "the gateway rejected the credential crier was configured with", status)
	case status < 200 || status > 299:
		return "", fail(CodeGatewayRejected, fmt.Sprintf("gateway answered %d creating a session: %s", status, snippet(raw)), status)
	}
	var out struct {
		Session gatewaySession `json:"session"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Session.ID != "" {
		return out.Session.ID, nil
	}
	if id := headerValue(header, "X-Hermes-Session-Id"); id != "" {
		return id, nil
	}
	return "", fail(CodeGatewayRejected, fmt.Sprintf("gateway created a session without an id: %s", snippet(raw)), status)
}

// headerValue reads one header from a saved response header map.
func headerValue(h http.Header, name string) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get(name))
}

// chat runs one turn in the session and returns the gateway's correlation id
// and the assistant's reply.
func (c *Client) chat(ctx context.Context, cfg *Config, token, sessionID string, req Request) (string, string, error) {
	body := map[string]any{"message": messageText(req.Body)}
	status, raw, header, err := c.do(ctx, cfg, token, http.MethodPost,
		"/api/sessions/"+sessionID+"/chat", body, req)
	if err != nil {
		return "", "", err
	}
	switch {
	case status == http.StatusNotFound:
		return "", "", fail(CodeSessionNotFound, fmt.Sprintf("session %q does not exist", sessionID), status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "", "", fail(CodeGatewayUnauthorized, "the gateway rejected the credential crier was configured with", status)
	case status < 200 || status > 299:
		return "", "", fail(CodeGatewayRejected, fmt.Sprintf("gateway answered %d in session %q: %s", status, sessionID, snippet(raw)), status)
	}
	var out struct {
		SessionID string `json:"session_id"`
		Message   struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(raw, &out)
	correlationID := out.SessionID
	if correlationID == "" {
		correlationID = headerValue(header, "X-Hermes-Session-Id")
	}
	if correlationID == "" {
		correlationID = sessionID
	}
	return correlationID, out.Message.Content, nil
}

// do performs one authenticated gateway call and returns its status and body.
// A transport-level failure (no answer at all) is HERMES_GATEWAY_UNREACHABLE.
func (c *Client) do(ctx context.Context, cfg *Config, token, method, path string, body any, req Request) (int, []byte, http.Header, error) {
	url := strings.TrimRight(cfg.BaseURL, "/") + path
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, fail(CodeGatewayRejected, "encode gateway request: "+err.Error(), 0)
		}
		reader = bytes.NewReader(encoded)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, nil, fail(CodeGatewayUnreachable, "build gateway request: "+err.Error(), 0)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// Correlation travels WITH the request so a Hermes-side turn can quote it
	// back: the message id is the id crier will match a reply against.
	if req.MessageID != "" {
		httpReq.Header.Set("X-Crier-Message-Id", req.MessageID)
	}
	if req.AgentID != "" {
		httpReq.Header.Set("X-Crier-Target", req.AgentID)
	}
	if req.Sender != "" {
		httpReq.Header.Set("X-Crier-Agent", req.Sender)
	}
	if req.Kind != "" {
		httpReq.Header.Set("X-Crier-Event", req.Kind)
	}
	if req.CrierSessionID != "" {
		httpReq.Header.Set("X-Crier-Session-Id", req.CrierSessionID)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// The URL and the error text carry no credential; the token only ever
		// travels in the Authorization header.
		return 0, nil, nil, fail(CodeGatewayUnreachable, fmt.Sprintf("call %s %s: %v", method, url, err), 0)
	}
	defer func() { _ = resp.Body.Close() }()
	header := resp.Header.Clone()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if readErr != nil {
		return resp.StatusCode, raw, header, fail(CodeGatewayUnreachable, fmt.Sprintf("read %s %s: %v", method, url, readErr), resp.StatusCode)
	}
	return resp.StatusCode, raw, header, nil
}

// readToken reads the bearer token from its file, enforcing the documented
// file discipline: a regular, non-empty file with NO group/other permissions.
// Errors name the path and the mode; the contents never leave this function.
func readToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("no token_file configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token file %q is unavailable: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("token file %q is not a regular file (mode %s)", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("token file %q is group/world-accessible (mode %04o); want 0600", path, perm)
	}
	if info.Size() > maxTokenBytes {
		return "", fmt.Errorf("token file %q is %d bytes; a bearer token is shorter than %d", path, info.Size(), maxTokenBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token file %q is unreadable: %v", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %q is empty", path)
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("token file %q holds more than one line", path)
	}
	return token, nil
}

// messageText renders a crier payload as the chat message a Hermes turn reads.
// A JSON string is decoded (the sender said exactly that text); anything else
// is sent as its JSON text, so a structured payload arrives readable rather
// than double-escaped or silently emptied.
func messageText(payload []byte) string {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err == nil {
			return text
		}
	}
	return string(trimmed)
}

// snippet bounds a gateway body in an error message.
func snippet(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "(empty body)"
	}
	const max = 200
	if len(trimmed) > max {
		return string(trimmed[:max]) + "…"
	}
	return string(trimmed)
}

// keyedLock is one message-id mutex with a reference count, so the map does not
// grow without bound.
type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// keyedLocks hands out one mutex per key.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

// lock acquires key's mutex and returns its release function.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyedLock)
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs <= 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
