// Package client is crier's OUTBOUND A2A client: the second half of
// specs/A2A-OPTION.md §1 — "lets a crier agent be reachable by an A2A client —
// and, later, lets a crier agent be an A2A client".
//
// Everything else in internal/a2a is the SERVER side: a card projection, a
// JSON-RPC binding, a task projection. This package is the other direction. It
// speaks the same wire — it reuses internal/a2a's AgentCard, Message, Part,
// Task, param structs, error codes and method names rather than restating
// them — and adds only what a caller needs that a server does not:
//
//   - discovery: fetch `/.well-known/agent-card.json` from a foreign origin and
//     refuse an absent, non-200 or malformed card with a NAMED error;
//   - credentials read from a file or an environment variable, redacted
//     everywhere, applied from the card's own securitySchemes;
//   - idempotency: one messageId per send, reused across a retry, with the
//     peer's own task id reported back so a retry cannot mint a second task;
//   - four failure paths that each answer a DISTINCT named error instead of a
//     generic "request failed".
//
// The client does NOT require CR_A2A_ENABLED on the local relay: that switch
// gates crier's own A2A SERVER routes (§4.1). Being an A2A client is a
// separate, outbound-only capability — a crier relay can address a foreign
// agent while its own A2A surface stays dark, which is exactly the posture the
// live fleet bus is in.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/buildinfo"
)

// DefaultTimeout is the per-request budget. It is deliberately the same
// fifteen seconds internal/registry's RemoteStore uses: an A2A peer is an
// ordinary HTTP service, and a client that waits longer than its server's own
// outbound budget cannot be reasoned about from one side.
const DefaultTimeout = 15 * time.Second

// DefaultMaxAttempts is one request plus at most one transport retry. The retry
// reuses the SAME messageId, so it cannot create a second task on the peer
// (§5.4.1a); it exists only because a connection reset before the response is
// indistinguishable from a send that never happened, and re-sending under the
// same idempotency key is the one safe way to find out.
const DefaultMaxAttempts = 2

// maxResponseBytes caps what the client will read from a peer. It matches the
// JSON-RPC binding's own request ceiling (4 MiB, §5.4) with headroom for a card
// carrying many skills.
const maxResponseBytes = 8 << 20

// maxMessageIDBytes is crier's own limit for the idempotency key a messageId
// becomes (§5.4.2), applied here so an over-long id is refused locally instead
// of by the peer with a -32602 that names crier's internal field.
const maxMessageIDBytes = 128

// versionHeader is A2A's protocol-version service parameter (§9.2, §14.2.1): a
// header, never a member of params.
const versionHeader = "A2A-Version"

// Stable, machine-readable names for the failures this client reports. The
// CLI prints these verbatim and uses them to pick its exit code, so a caller
// (or an operator reading a log) can tell the four failure paths apart without
// parsing prose.
const (
	// NamePeerUnreachable: the peer could not be reached at all — connection
	// refused, DNS failure, TLS failure, timeout.
	NamePeerUnreachable = "PEER_UNREACHABLE"
	// NameCardNotFound: the discovery path answered 404 (no card there).
	NameCardNotFound = "AGENT_CARD_NOT_FOUND"
	// NameCardMalformed: something answered, but not a usable AgentCard.
	NameCardMalformed = "AGENT_CARD_MALFORMED"
	// NameInvalidResponse: the peer's body is not a valid JSON-RPC 2.0
	// response correlated with the request.
	NameInvalidResponse = "INVALID_JSONRPC_RESPONSE"
	// NameUnauthenticated: the peer answered 401/403. ONE error, no retry.
	NameUnauthenticated = "UNAUTHENTICATED"
	// NameCredentialRequired: the card requires a scheme this client holds no
	// credential for — refused BEFORE anything is sent.
	NameCredentialRequired = "CREDENTIAL_REQUIRED"
	// NamePeerRefused: the peer answered a well-formed JSON-RPC error.
	NamePeerRefused = "PEER_REFUSED"
	// NamePeerHTTPError: the peer answered an HTTP error that is not an auth
	// answer (5xx, 405, 415, …) rather than a JSON-RPC response.
	NamePeerHTTPError = "PEER_HTTP_ERROR"
	// NameCredentialInvalid: a credential could not be read (missing file,
	// blank value, world-readable file, unset variable).
	NameCredentialInvalid = "CREDENTIAL_INVALID"
	// NameConfigInvalid: the client was built with something it cannot use.
	NameConfigInvalid = "CONFIG_INVALID"
)

// The sentinels behind those names. errors.Is works against them; the *Error
// also carries the peer's status and the JSON-RPC error object when there is
// one, so a caller can inspect without string matching.
var (
	ErrPeerUnreachable    = errors.New("a2a client: peer unreachable")
	ErrCardNotFound       = errors.New("a2a client: agent card not found")
	ErrCardMalformed      = errors.New("a2a client: agent card malformed")
	ErrInvalidResponse    = errors.New("a2a client: invalid JSON-RPC response")
	ErrUnauthenticated    = errors.New("a2a client: unauthenticated")
	ErrCredentialRequired = errors.New("a2a client: credential required")
	ErrPeerRefused        = errors.New("a2a client: peer refused the request")
	ErrPeerHTTPError      = errors.New("a2a client: peer answered an HTTP error")
	ErrCredentialInvalid  = errors.New("a2a client: invalid credential")
	ErrConfigInvalid      = errors.New("a2a client: invalid configuration")
)

// names maps each sentinel to its stable name.
var names = map[error]string{
	ErrPeerUnreachable:    NamePeerUnreachable,
	ErrCardNotFound:       NameCardNotFound,
	ErrCardMalformed:      NameCardMalformed,
	ErrInvalidResponse:    NameInvalidResponse,
	ErrUnauthenticated:    NameUnauthenticated,
	ErrCredentialRequired: NameCredentialRequired,
	ErrPeerRefused:        NamePeerRefused,
	ErrPeerHTTPError:      NamePeerHTTPError,
	ErrCredentialInvalid:  NameCredentialInvalid,
	ErrConfigInvalid:      NameConfigInvalid,
}

// Error is the client's failure type: which operation failed, which of the
// named failures it is, the HTTP status the peer answered (0 when the peer was
// never reached), and the peer's own JSON-RPC error when it sent one.
//
// It carries no credential material: nothing here is built from a secret, and
// RPC (the peer's error object) is the peer's own text.
type Error struct {
	// Op is the operation that failed: "discover", "send", "getTask",
	// "cancelTask", "credential", "config".
	Op string
	// Name is the stable name from the constants above.
	Name string
	// Kind is the sentinel this failure belongs to.
	Kind error
	// Status is the HTTP status the peer answered, 0 if it was never reached.
	Status int
	// Detail explains the failure in one sentence.
	Detail string
	// RPC is the peer's JSON-RPC error object, when it answered one.
	RPC *a2a.RPCError
	// Err is the underlying cause (a transport error, a decode error) — kept
	// for errors.Is/As, never required to understand the failure.
	Err error
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("a2a client")
	if e.Op != "" {
		b.WriteString(" ")
		b.WriteString(e.Op)
	}
	b.WriteString(": ")
	b.WriteString(e.Name)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.Status)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Unwrap exposes both the sentinel and the underlying cause, so
// errors.Is(err, ErrUnauthenticated) and errors.As(err, &url.Error{}) both work.
func (e *Error) Unwrap() []error {
	out := []error{e.Kind}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// newError builds an *Error. An empty op is allowed for configuration-stage
// failures, where no operation ran.
func newError(op, name, detail string, cause error) *Error {
	return &Error{Op: op, Name: name, Kind: kindFor(name), Detail: detail, Err: cause}
}

// kindFor reverses names, so the constructors cannot drift from the table.
func kindFor(name string) error {
	for kind, n := range names {
		if n == name {
			return kind
		}
	}
	return ErrConfigInvalid
}

// ErrorName reports the stable name of err: the *Error's own Name when it is
// one of this package's failures, or "" for anything else.
func ErrorName(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Name
	}
	return ""
}

// Options configures a Client.
type Options struct {
	// Origin is the peer's scheme://host[:port][/path]. Required.
	Origin string
	// AgentID is the `agent_id` discovery selector. A crier relay serves many
	// agents behind one origin and needs it (§5.3); a peer that IS one agent
	// serves its card at the bare well-known path and needs no selector, so an
	// empty AgentID sends no query parameter at all.
	AgentID string
	// Credentials are the secrets this client may present. They come from
	// CredentialFromFile / CredentialFromEnv — never from a value passed in
	// from a log line or a command-line argument.
	Credentials []Credential
	// HTTPClient is the transport. Nil means an http.Client with Timeout.
	HTTPClient *http.Client
	// Timeout is the per-request budget when HTTPClient is nil.
	Timeout time.Duration
	// Methods is the wire dialect. Nil means WireV1 (A2A v1.0.0 §9.1 — and
	// crier's own server). Use &WireLegacy for a pre-1.0 peer.
	Methods *Operations
	// MaxAttempts is the send ceiling, 1 means "never retry". Zero means
	// DefaultMaxAttempts.
	MaxAttempts int
}

// Client talks the A2A JSON-RPC binding to ONE peer origin.
type Client struct {
	origin     string
	agentID    string
	creds      []Credential
	httpClient *http.Client
	methods    Operations
	attempts   int
}

// New validates the options and returns a Client. It performs no I/O: discovery
// is an explicit call, so a caller can build a client and decide what to
// address.
func New(opts Options) (*Client, error) {
	origin := strings.TrimRight(strings.TrimSpace(opts.Origin), "/")
	if origin == "" {
		return nil, newError("config", NameConfigInvalid, "an origin is required (scheme://host[:port])", nil)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil, newError("config", NameConfigInvalid, fmt.Sprintf("origin %q is not a URL: %v", origin, err), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, newError("config", NameConfigInvalid, fmt.Sprintf(
			"origin %q must be http:// or https:// (got %q)", origin, u.Scheme), nil)
	}
	if u.Host == "" {
		return nil, newError("config", NameConfigInvalid, fmt.Sprintf("origin %q has no host", origin), nil)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, newError("config", NameConfigInvalid, fmt.Sprintf(
			"origin %q must not carry a query or fragment", origin), nil)
	}

	methods := WireV1
	if opts.Methods != nil {
		methods = *opts.Methods
	}
	if err := methods.validate(); err != nil {
		return nil, err
	}

	attempts := opts.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	if attempts > 3 {
		attempts = 3
	}

	hc := opts.HTTPClient
	if hc == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}

	creds := make([]Credential, 0, len(opts.Credentials))
	for _, c := range opts.Credentials {
		if strings.TrimSpace(c.Scheme) == "" || c.secret == "" {
			return nil, newError("config", NameConfigInvalid,
				"a credential was passed without a scheme name or a value", nil)
		}
		creds = append(creds, c)
	}

	return &Client{
		origin:     origin,
		agentID:    strings.TrimSpace(opts.AgentID),
		creds:      creds,
		httpClient: hc,
		methods:    methods,
		attempts:   attempts,
	}, nil
}

// Origin is the peer origin this client addresses.
func (c *Client) Origin() string { return c.origin }

// Methods is the wire dialect in force.
func (c *Client) Methods() Operations { return c.methods }

// userAgent identifies the client without leaking anything about the caller.
func (c *Client) userAgent() string {
	return "crier-a2a-client/" + buildinfo.String()
}

// newID mints a corrrelation id for a JSON-RPC request: crypto/rand hex, so two
// concurrent clients never collide on one peer.
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition this client can paper over:
		// fall back to a timestamp-derived id, which is still unique enough
		// within one request and is never a security boundary.
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// post sends one JSON-RPC request to endpoint and returns the raw `result`
// member of the response.
//
// The envelope is internal/a2a's own RPCRequest, so the bytes on the wire are
// exactly the shape crier's own server decodes (jsonrpc/method/id/params, with
// DisallowUnknownFields). The response is read through a thin local struct
// because `result` must stay RAW: each operation decodes it into the server's
// own result type (a2a.SendMessageResponse, a2a.Task), and decoding into `any`
// first would lose the exact bytes.
func (c *Client) post(ctx context.Context, op string, card *Card, method string, params any) (json.RawMessage, error) {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, newError(op, NameConfigInvalid, "cannot encode the request parameters: "+err.Error(), err)
	}
	reqID := newID("req")
	envelope := a2a.RPCRequest{
		JSONRPC: a2a.JSONRPCVersion,
		ID:      json.RawMessage(strconvQuote(reqID)),
		Method:  method,
		Params:  rawParams,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, newError(op, NameConfigInvalid, "cannot encode the JSON-RPC request: "+err.Error(), err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, card.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, newError(op, NameConfigInvalid, "cannot build the request: "+err.Error(), err)
	}
	req.Header.Set("Content-Type", a2a.RPCMediaType)
	req.Header.Set("Accept", a2a.RPCMediaType)
	req.Header.Set(versionHeader, a2a.ProtocolVersion)
	req.Header.Set("User-Agent", c.userAgent())
	if err := c.authorize(req, &card.AgentCard); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, unreachable(op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus(op, method, resp.StatusCode); err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, err
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, unreachable(op, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, newError(op, NameInvalidResponse, fmt.Sprintf(
			"the peer's response exceeds the %d-byte ceiling", maxResponseBytes), nil)
	}

	var decoded rpcResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, newError(op, NameInvalidResponse, fmt.Sprintf(
			"the peer's body is not JSON: %v", err), err)
	}
	if decoded.JSONRPC != a2a.JSONRPCVersion {
		return nil, newError(op, NameInvalidResponse, fmt.Sprintf(
			"the peer's body is not a JSON-RPC %s response (jsonrpc=%q)", a2a.JSONRPCVersion, decoded.JSONRPC), nil)
	}
	if got := strings.TrimSpace(string(decoded.ID)); got != strconvQuote(reqID) {
		return nil, newError(op, NameInvalidResponse, fmt.Sprintf(
			"the peer's response id %s does not echo the request id %s", describeRawID(got), reqID), nil)
	}
	hasResult := len(bytes.TrimSpace(decoded.Result)) > 0
	if hasResult == (decoded.Error != nil) {
		return nil, newError(op, NameInvalidResponse,
			"the peer's response must carry exactly one of result or error", nil)
	}
	if decoded.Error != nil {
		return nil, peerRefused(op, method, decoded.Error)
	}
	return decoded.Result, nil
}

// rpcResponse is the client-side decode target for a JSON-RPC response
// envelope: the same three members the server writes (jsonrpc, id, and exactly
// one of result/error), with `result` kept as raw bytes.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *a2a.RPCError   `json:"error"`
}

// unreachable classifies a transport failure. A context cancellation is
// reported as unreachable too, but the cause is kept so a caller that cancelled
// deliberately can see errors.Is(err, context.Canceled).
func unreachable(op string, err error) *Error {
	detail := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		detail = "the peer did not answer within the client's timeout (" + detail + ")"
	}
	e := newError(op, NamePeerUnreachable, detail, err)
	return e
}

// classifyStatus turns a non-200 HTTP answer into a named failure. 401 and 403
// are ONE named failure (ErrUnauthenticated) and are never retried: a peer that
// refused the credential will refuse it again, and hammering it is a retry
// storm, not resilience.
func classifyStatus(op, method string, status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		e := newError(op, NameUnauthenticated, fmt.Sprintf(
			"the peer refused %s with HTTP %d — check the credential's scheme name and value", method, status), nil)
		e.Status = status
		return e
	default:
		e := newError(op, NamePeerHTTPError, fmt.Sprintf(
			"the peer answered %s with HTTP %d, not a JSON-RPC response", method, status), nil)
		e.Status = status
		return e
	}
}

// peerRefused wraps the peer's own JSON-RPC error object.
func peerRefused(op, method string, rpcErr *a2a.RPCError) *Error {
	e := newError(op, NamePeerRefused, fmt.Sprintf(
		"the peer refused %s: %d %s", method, rpcErr.Code, rpcErr.Message), nil)
	e.RPC = rpcErr
	return e
}

// strconvQuote renders s as a JSON string literal, which is the form an id
// takes on the wire.
func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// describeRawID keeps a mismatched id readable in an error without echoing an
// unbounded body.
func describeRawID(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

// marshalJSON is a one-line indirection so credential.go can serialize its
// redacted view without importing encoding/json itself.
func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
