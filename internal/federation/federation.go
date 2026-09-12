package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Federation (CR-FEAT-006): relay-to-relay links. A relay configured with
// CR_FED_LINKS can route deliveries to agents registered on linked relays
// (the deliver request hops via the link) and advertise cross-relay agent
// discovery through GET /fed/peers. No full mesh is required: links are
// explicit and one-directional (relay A linking to B does not imply B links
// back), and a hop marker prevents delivery loops when two relays link to
// each other.

// HopHeader marks a request as already forwarded over a relay link. A relay
// receiving a request with this header never forwards it again — the agent
// either exists locally or the request 404s (loop prevention for mutual
// links).
const HopHeader = "X-Crier-Fed-Hop"

// BearerPrefix is the authorization scheme used for federation link auth
// (DF-CRIER-6). The shared secret is sent as "Authorization: Bearer <token>",
// matching the Bearer scheme the destination relay's CR_AUTH_TOKEN middleware
// already validates.
const BearerPrefix = "Bearer "

// DefaultTimeout bounds a single outbound federation HTTP request. Blocking
// webhook deliveries on the remote relay run within their own budget
// (default 30s), so 60s comfortably covers the remote round-trip.
const DefaultTimeout = 60 * time.Second

// Link is one relay-to-relay link target parsed from CR_FED_LINKS.
type Link struct {
	// URL is the base URL of the linked relay, e.g. "http://localhost:18772"
	// (trailing slash trimmed).
	URL string
	// Name is the display name used in the /fed/peers listing; defaults to
	// the link URL's host:port.
	Name string
}

// RemoteAgent is a slim agent summary used for cross-relay discovery.
// Only id and capabilities cross the link; local details (keys, webhook
// config) stay on the owning relay.
type RemoteAgent struct {
	ID           string   `json:"id"`
	Capabilities []string `json:"capabilities"`
}

// Peer is one relay in the federation listing served by GET /fed/peers:
// the local relay first, then each linked relay with its agents fetched
// live from the linked relay's GET /agents.
type Peer struct {
	Name   string        `json:"name"`
	URL    string        `json:"url"`
	Agents []RemoteAgent `json:"agents"`
}

// ErrNotFoundOnAnyLink reports that every configured linked relay answered
// 404, i.e. the agent exists nowhere in the federation. It is a DEFINITIVE
// answer and never wraps a link outage: a transient failure (unreachable
// link, retryable status) is reported as *TransientError instead, so callers
// can tell "the agent is nowhere" (answer 404) from "the federation is
// unreachable" (hold and retry, spec §8, DF-CRIER-7).
var ErrNotFoundOnAnyLink = errors.New("agent not found on any linked relay")

// TransientError reports that at least one configured link failed
// transiently (transport error or retryable status) and no link delivered
// the request. It carries the last observed failure for the terminal
// FEDERATION_FAILED report. Callers must not answer 404 for it.
type TransientError struct {
	// Attempts is the number of forward passes made in this call (always 1
	// for a single pass; the hold queue keeps counting across retries).
	Attempts int
	// LastStatus is the last retryable remote status seen (0 when the
	// failure was transport-level).
	LastStatus int
	// LastErr is the last transport-level error (nil when every link
	// answered with a retryable status).
	LastErr error
}

// Error implements error. The message carries the link URL and the transport
// error only — never the link token.
func (e *TransientError) Error() string {
	switch {
	case e.LastErr != nil && e.LastStatus != 0:
		return fmt.Sprintf("federation: no link delivered (last status %d, last error: %v)", e.LastStatus, e.LastErr)
	case e.LastErr != nil:
		return fmt.Sprintf("federation: no link reachable (last error: %v)", e.LastErr)
	case e.LastStatus != 0:
		return fmt.Sprintf("federation: no link delivered (last status %d)", e.LastStatus)
	default:
		return "federation: no link delivered"
	}
}

// Unwrap exposes the underlying transport error.
func (e *TransientError) Unwrap() error { return e.LastErr }

// retryableStatus reports whether a remote link response means "the link is
// unhealthy, try again later" rather than "here is the answer to your
// delivery":
//
//   - 5xx: the gateway class a half-dead relay, proxy or load balancer emits
//     (the repo's webhook client classifies every 5xx as retryable, spec §9
//     "transient retries", so the relay link follows the same rule);
//   - 408 Request Timeout / 429 Too Many Requests: explicit "try again".
//
// Everything else — 2xx/3xx (relayed verbatim), 404 (try the next link),
// other 4xx (definitive rejection) — is an answer, not a link outage.
//
// Caveat: retrying after a 5xx can duplicate a delivery the remote relay
// partially processed, exactly as the webhook client's retry-on-5xx already
// can. The duplicate window is the reason the recovery pass stops at the
// first non-retryable answer.
func retryableStatus(status int) bool {
	switch {
	case status >= 500:
		return true
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// Client forwards deliveries and discovery to the configured linked relays.
// When token is non-empty, every outbound request carries
// "Authorization: Bearer <token>" (federation link auth, DF-CRIER-6) so a
// CR_AUTH_TOKEN-protected destination relay accepts the forward. The token
// is write-protected: it is never logged, echoed, serialized, or included
// in /fed/peers output. The zero value is not usable; construct with
// NewClient.
type Client struct {
	links []Link
	token string
	http  *http.Client
	// hold queues deliveries whose links are all transiently down for
	// bounded retry (DF-CRIER-7, spec §8). Nil means a transient failure is
	// reported to the caller synchronously (HTTP 502 FEDERATION_FAILED)
	// instead of being held.
	hold *HoldManager
}

// SetHoldManager attaches the hold/retry queue (DF-CRIER-7). With no hold
// manager a transient link outage is surfaced to the caller as an explicit
// bounded failure rather than held (never as a 404).
func (c *Client) SetHoldManager(m *HoldManager) { c.hold = m }

// MaxHold returns the hold budget in force, or 0 when no hold queue is
// attached.
func (c *Client) MaxHold() time.Duration {
	if c.hold == nil {
		return 0
	}
	return c.hold.MaxHold()
}

// timeout is the outbound per-request budget.
func (c *Client) timeout() time.Duration {
	if c.http != nil && c.http.Timeout > 0 {
		return c.http.Timeout
	}
	return DefaultTimeout
}

// NewClient builds a Client over the link base URLs from CR_FED_LINKS.
// Entries that are not valid http(s) URLs are skipped (they are config
// garbage; the remaining links still work). A non-positive timeout falls
// back to DefaultTimeout. token is the optional shared secret for link
// authentication ("" = unauthenticated links, CR_FED_TOKEN unset).
func NewClient(links []string, timeout time.Duration, token string) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := &Client{http: &http.Client{Timeout: timeout}, token: token}
	for _, raw := range links {
		if l, ok := parseLink(raw); ok {
			c.links = append(c.links, l)
		}
	}
	return c
}

// Links returns the configured (valid) links.
func (c *Client) Links() []Link {
	return c.links
}

// authorize sets the Authorization header on an outbound request when the
// client was configured with a shared secret (CR_FED_TOKEN). With no token
// the header is left unset entirely (unauthenticated links, the historical
// behavior). Never log the header or the token.
func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", BearerPrefix+c.token)
	}
}

// parseLink normalizes one CR_FED_LINKS entry into a Link. Valid entries
// are http(s) URLs with a host; the display name defaults to host:port.
func parseLink(raw string) (Link, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Link{}, false
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return Link{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Link{}, false
	}
	return Link{URL: strings.TrimSuffix(raw, "/"), Name: u.Host}, true
}

// ForwardDeliver POSTs a deliver request body to the linked relay's inbox
// endpoint: <link>/agents/<agentID>/inbox. The body is the exact deliver
// JSON the caller received (payload, sender, session_id, delivery_mode,
// timeout_ms, request_id, kind), so the remote relay's response — including
// a blocking webhook reply — is semantically identical to a local delivery.
// Returns the remote status and body verbatim; err is non-nil only for
// transport-level failures (unreachable link, timeout).
func (c *Client) ForwardDeliver(ctx context.Context, link Link, agentID string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		link.URL+"/agents/"+url.PathEscape(agentID)+"/inbox", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("federation: build forward request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HopHeader, "1")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("federation: forward to %s: %w", link.URL, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("federation: read response from %s: %w", link.URL, err)
	}
	return resp.StatusCode, respBody, nil
}

// forwardPass makes one pass over the links and returns the first non-404,
// non-retryable response verbatim. A 404 means "this relay does not know the
// agent either" — the search continues. When nothing delivered, the returned
// error is either *TransientError (at least one link failed transiently) or
// ErrNotFoundOnAnyLink (every link answered 404 / no links configured), so
// the caller can hold-and-retry the former and answer 404 for the latter.
func (c *Client) forwardPass(ctx context.Context, agentID string, body []byte) (int, []byte, error) {
	var (
		lastErr      error
		lastStatus   int
		sawTransient bool
	)
	for _, link := range c.links {
		status, respBody, err := c.ForwardDeliver(ctx, link, agentID, body)
		if err != nil {
			slog.Warn("federation: link unreachable, trying next", "link", link.URL, "error", err)
			lastErr = err
			sawTransient = true
			continue
		}
		if status == http.StatusNotFound {
			continue // agent not on this relay either
		}
		if retryableStatus(status) {
			slog.Warn("federation: link unhealthy, trying next", "link", link.URL, "status", status)
			lastStatus = status
			sawTransient = true
			continue
		}
		return status, respBody, nil
	}
	if sawTransient {
		return 0, nil, &TransientError{Attempts: 1, LastStatus: lastStatus, LastErr: lastErr}
	}
	return 0, nil, ErrNotFoundOnAnyLink
}

// ForwardToAny relays a deliver request to each linked relay in order and
// returns the first non-404, non-retryable response verbatim. When nothing
// delivered it returns *TransientError or ErrNotFoundOnAnyLink (see
// forwardPass); callers that must not lose a message on a link outage use
// ForwardOrHold instead.
func (c *Client) ForwardToAny(ctx context.Context, agentID string, body []byte) (int, []byte, error) {
	return c.forwardPass(ctx, agentID, body)
}

// ForwardOrHold makes one immediate pass over the links — the synchronous
// behavior callers already depend on: a successful forward (including a
// blocking webhook reply) is returned verbatim (err == nil, held == nil), and
// a definitive all-links-404 is returned as ErrNotFoundOnAnyLink (the caller
// answers 404).
//
// A transient failure (every link unreachable / retryable) instead hands the
// delivery to the hold queue when one is attached, and returns the held item
// so the caller can accept it (HTTP 202) rather than lose it — the returned
// error is still non-nil, so only err == nil means "relayed". With no queue
// the transient error is returned unchanged (held == nil), and the caller
// reports an explicit synchronous failure (HTTP 502) — never a 404.
func (c *Client) ForwardOrHold(ctx context.Context, agentID string, body []byte, meta HoldMeta) (int, []byte, *HoldItem, error) {
	status, respBody, err := c.forwardPass(ctx, agentID, body)
	if err == nil {
		return status, respBody, nil, nil
	}
	var transient *TransientError
	if !errors.As(err, &transient) {
		return 0, nil, nil, err // definitive: agent not found anywhere
	}
	if c.hold == nil {
		return 0, nil, nil, err
	}
	item := &HoldItem{
		ID:         meta.MessageID,
		AgentID:    agentID,
		Body:       append(json.RawMessage(nil), body...),
		Sender:     meta.Sender,
		RequestID:  meta.RequestID,
		SessionID:  meta.SessionID,
		Attempts:   transient.Attempts,
		LastStatus: transient.LastStatus,
		LastError:  transient.Error(),
	}
	if qerr := c.hold.Enqueue(item); qerr != nil {
		// Queue full / unusable body / write failure: report synchronously
		// rather than pretend the message was accepted.
		slog.Warn("federation: hold queue refused delivery", "target", agentID, "error", qerr)
		return 0, nil, nil, fmt.Errorf("federation: hold delivery: %w", qerr)
	}
	return 0, nil, item, err
}

// FetchRemoteAgents fetches the agent list from a linked relay via its
// GET /agents endpoint (cross-relay agent discovery, CR-FEAT-006). Unknown
// fields in the remote agent objects are ignored; only id and capabilities
// are kept.
func (c *Client) FetchRemoteAgents(ctx context.Context, link Link) ([]RemoteAgent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL+"/agents", nil)
	if err != nil {
		return nil, fmt.Errorf("federation: build agents request: %w", err)
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("federation: fetch agents from %s: %w", link.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("federation: GET %s/agents: status %d", link.URL, resp.StatusCode)
	}
	var out struct {
		Agents []RemoteAgent `json:"agents"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("federation: decode agents from %s: %w", link.URL, err)
	}
	return out.Agents, nil
}

// Peers builds the live federation listing for GET /fed/peers: one Peer per
// linked relay, agents fetched live. A link that fails to answer still
// appears in the listing (with empty agents) so operators can see the link
// exists but is down.
func (c *Client) Peers(ctx context.Context) []Peer {
	peers := make([]Peer, 0, len(c.links))
	for _, link := range c.links {
		p := Peer{Name: link.Name, URL: link.URL, Agents: []RemoteAgent{}}
		agents, err := c.FetchRemoteAgents(ctx, link)
		if err != nil {
			slog.Warn("federation: peers fetch failed", "link", link.URL, "error", err)
		} else {
			p.Agents = agents
		}
		peers = append(peers, p)
	}
	return peers
}

// HandlePeers serves GET /fed/peers: the federation listing. The local
// relay is included first (local is a live closure — agents are read from
// the registry at request time), followed by each linked relay fetched
// live. A nil client (no links configured) serves the local entry alone.
func HandlePeers(c *Client, local func() Peer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peers := []Peer{local()}
		if c != nil {
			peers = append(peers, c.Peers(r.Context())...)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"peers": peers})
	}
}
