package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/crier-dev/crier/internal/a2a"
)

// Card is a peer's discovered AgentCard plus the facts this client needs to
// address it: the endpoint the card advertises, the tenant to send back, and a
// digest of the exact bytes the peer served.
//
// AgentCard is internal/a2a's own type — the SAME one the publisher projects
// (§5.3). There is no second card model here: a field crier's card cannot
// express is not invented, and a field crier's card adds later is readable
// without touching this package.
type Card struct {
	// AgentCard is the parsed card.
	AgentCard a2a.AgentCard
	// Origin is the origin the card was fetched from.
	Origin string
	// URL is the exact discovery URL that answered.
	URL string
	// Endpoint is the JSON-RPC binding URL the card advertises
	// (`supportedInterfaces[]` entry with protocolBinding JSONRPC).
	Endpoint string
	// Binding is that interface's protocolBinding (`JSONRPC`).
	Binding string
	// ProtocolVersion is that interface's protocolVersion (`1.0`).
	ProtocolVersion string
	// Tenant is what `params.tenant` must carry for a multi-agent origin: the
	// interface's own `tenant` (crier sets it to the registry id, §5.3), else
	// the discovery selector, else the card's name.
	Tenant string
	// Digest is "sha256:<hex>" over the exact response bytes. CardETag is the
	// peer's own ETag header when it sent one.
	Digest   string
	CardETag string
	// Bytes is the card body as served.
	Bytes []byte
}

// DigestOf returns the "sha256:<hex>" digest of body — the digest this client
// records for the card it addressed. It is the same hash the publisher's ETag
// is derived from (§5.3.2's CardETag truncates it to 128 bits), so an operator
// can compare a recorded digest against a served validator without a second
// tool.
func DigestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Discover fetches the peer's AgentCard and refuses anything that is not a
// usable card, each with its own named error:
//
//	404                                  → AGENT_CARD_NOT_FOUND
//	401 / 403                            → UNAUTHENTICATED (one error, no retry)
//	transport failure                    → PEER_UNREACHABLE
//	non-JSON, missing required fields,
//	no JSON-RPC interface                → AGENT_CARD_MALFORMED
//	a declared security requirement this
//	client holds no credential for       → CREDENTIAL_REQUIRED
//
// The last one is checked HERE rather than at the first send, so a peer that
// would refuse every request anyway is refused before this client sends
// anything at all (§5.3.1's posture table: a card can require bearerAuth).
func (c *Client) Discover(ctx context.Context) (*Card, error) {
	cardURL := c.cardURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, newError("discover", NameConfigInvalid, "cannot build the discovery request: "+err.Error(), err)
	}
	req.Header.Set("Accept", a2a.CardMediaType+", "+a2a.RPCMediaType)
	req.Header.Set("User-Agent", c.userAgent())
	// Before the card is read, the only scheme name this client can recognise
	// is crier's own (§5.3.1): a crier origin requires the same bearer header
	// on the discovery route it requires everywhere else, and it is NOT on the
	// auth-exempt list. An alternate scheme name is applied from the card, one
	// step later, for the operations that follow.
	c.applyDiscoveryCredential(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, unreachable("discover", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := cardStatus(resp.StatusCode); err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, unreachable("discover", err)
	}
	if len(body) > maxResponseBytes {
		return nil, newError("discover", NameCardMalformed, fmt.Sprintf(
			"the agent card exceeds the %d-byte ceiling", maxResponseBytes), nil)
	}

	card, err := parseCard(body)
	if err != nil {
		return nil, err
	}
	card.Origin = c.origin
	card.URL = cardURL
	card.Digest = DigestOf(body)
	card.Bytes = body
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
		card.CardETag = etag
	}
	if card.Tenant == "" {
		card.Tenant = c.agentID
	}
	if card.Tenant == "" {
		card.Tenant = card.AgentCard.Name
	}

	// A card that requires a scheme we cannot present is refused before any
	// operation is attempted.
	if _, satisfied := c.matchedCredentials(&card.AgentCard); !satisfied {
		return nil, credentialRequired(&card.AgentCard)
	}
	return card, nil
}

// cardStatus classifies the discovery response's HTTP status. Only these
// statuses are classified differently from a send: a card route that answers
// 404 means "no card here", which is a distinct named failure from a binding
// that answers 404 (§5.3's table: 404 = not a row, or the row did not opt in).
func cardStatus(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		e := newError("discover", NameCardNotFound,
			"the peer serves no agent card at the well-known path (HTTP 404)", nil)
		e.Status = status
		return e
	case http.StatusUnauthorized, http.StatusForbidden:
		e := newError("discover", NameUnauthenticated, fmt.Sprintf(
			"the peer refused discovery with HTTP %d — the card route is authenticated, so this client needs a credential before it can read one", status), nil)
		e.Status = status
		return e
	default:
		e := newError("discover", NamePeerHTTPError, fmt.Sprintf(
			"the peer answered discovery with HTTP %d", status), nil)
		e.Status = status
		return e
	}
}

// parseCard turns the served bytes into a Card whose endpoint is the JSON-RPC
// interface the card advertises.
//
// Validation is deliberately about USABILITY, not about matching crier's own
// card exactly: a foreign card legitimately carries members crier's type does
// not model (provider, iconUrl, signatures, …) and they are dropped by the
// non-strict decode — but a body that is not a JSON object, or that omits a
// field this client cannot work without, is MALFORMED rather than silently
// defaulted.
func parseCard(body []byte) (*Card, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, newError("discover", NameCardMalformed, "the agent card body is empty", nil)
	}
	if trimmed[0] != '{' {
		return nil, newError("discover", NameCardMalformed, fmt.Sprintf(
			"the agent card body is not a JSON object (starts with %q)", trimmed[0]), nil)
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		return nil, newError("discover", NameCardMalformed, "the agent card is not valid JSON: "+err.Error(), err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(trimmed, &card); err != nil {
		return nil, newError("discover", NameCardMalformed, "the agent card cannot be read: "+err.Error(), err)
	}

	if _, ok := members["name"]; !ok || strings.TrimSpace(card.Name) == "" {
		return nil, newError("discover", NameCardMalformed, "the agent card has no `name` (§4.4.1 marks it REQUIRED)", nil)
	}
	if _, ok := members["supportedInterfaces"]; !ok || len(card.SupportedInterfaces) == 0 {
		return nil, newError("discover", NameCardMalformed,
			"the agent card declares no `supportedInterfaces` — there is nowhere to send a request", nil)
	}

	out := &Card{AgentCard: card}
	var rejected []string
	for _, iface := range card.SupportedInterfaces {
		binding := strings.TrimSpace(iface.ProtocolBinding)
		// An absent protocolBinding means the JSON-RPC binding: it is the
		// only one this client speaks and the spec's default.
		if binding != "" && !strings.EqualFold(binding, a2a.ProtocolBindingJSONRPC) {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(iface.URL))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			rejected = append(rejected, iface.URL)
			continue
		}
		out.Endpoint = strings.TrimSpace(iface.URL)
		out.Binding = a2a.ProtocolBindingJSONRPC
		out.ProtocolVersion = strings.TrimSpace(iface.ProtocolVersion)
		out.Tenant = strings.TrimSpace(iface.Tenant)
		break
	}
	if out.Endpoint == "" {
		msg := "the agent card advertises no JSON-RPC interface"
		if len(rejected) > 0 {
			msg += " (interfaces with an unusable url: " + strings.Join(rejected, ", ") + ")"
		}
		return nil, newError("discover", NameCardMalformed, msg, nil)
	}
	return out, nil
}

// cardURL is the discovery URL: the well-known path, with the agent_id selector
// only when this client was given one (a single-agent peer serves the bare
// path).
func (c *Client) cardURL() string {
	u := c.origin + a2a.AgentCardPath
	if c.agentID == "" {
		return u
	}
	return u + "?" + a2a.AgentCardAgentQueryParam + "=" + url.QueryEscape(c.agentID)
}

// applyDiscoveryCredential attaches the one credential this client can place
// BEFORE a card has been read: crier's own bearerAuth (§5.3.1). Any other
// scheme name is applied from the card's own securitySchemes once the card is
// in hand.
func (c *Client) applyDiscoveryCredential(req *http.Request) {
	for _, cred := range c.creds {
		if cred.Scheme == bearerAuthSchemeName {
			req.Header.Set("Authorization", "Bearer "+cred.secret)
			return
		}
	}
}

// credentialRequired names the requirement the client cannot meet. The schemes
// it lists are the peer's own names, so the fix ("give me a credential for X")
// is actionable.
func credentialRequired(card *a2a.AgentCard) *Error {
	var required []string
	for _, req := range card.SecurityRequirements {
		var names []string
		for name := range req.Schemes {
			names = append(names, name)
		}
		if len(names) > 0 {
			required = append(required, strings.Join(sortedStrings(names), "+"))
		}
	}
	return newError("discover", NameCredentialRequired, fmt.Sprintf(
		"the agent card requires %s and this client holds no credential for it",
		strings.Join(sortedStrings(required), " or ")), nil)
}
