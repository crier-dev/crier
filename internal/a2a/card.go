package a2a

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The A2A Agent Card wire contract (INT-A2A-002, specs/A2A-OPTION.md §5.2).
//
// This file holds the PROJECTION of one opted-in crier registry row into an
// A2A v1.0.0 AgentCard (spec §4.4.1) plus the discovery constants the server
// registers a route from. It deliberately knows nothing about the registry
// package: the registry imports THIS one for the per-agent `a2a` block, so a
// dependency the other way would be a cycle — the caller passes the row's
// evidence in as CardRow.
//
// Wire facts that are decisions, not accidents (each is argued where it is
// used): field names are camelCase (§5.5), the media type is the A2A
// registration application/a2a+json (§14.1.1), the JSON-RPC binding is the
// only interface advertised (§2 of specs/A2A-OPTION.md), and the card carries
// no provider and no signatures — crier has neither, and inventing either
// would be a claim an A2A client could not verify.

const (
	// AgentCardPath is the A2A well-known discovery path (§8.2, §14.3) and the
	// ONE route INT-A2A-002 registers — only while CR_A2A_ENABLED is set.
	//
	// It is the path, verbatim: the spec registers `agent-card.json` under
	// `/.well-known/`, and the pre-1.0 path `/.well-known/agent.json` is NOT
	// aliased (crier implements the binding §2 decision — A2A v1.0.0 — and a
	// compatibility redirect would be a surface nobody asked for).
	AgentCardPath = "/.well-known/agent-card.json"

	// AgentCardAgentQueryParam names the registry row whose card to serve.
	//
	// The spec's discovery URL identifies ONE agent, because in the spec a
	// server IS an agent. A crier relay is a bus: many agents share one
	// origin, so the well-known path alone cannot name one. The parameter is
	// crier's own vocabulary (the `{id}` of GET /agents/{id}) rather than a
	// renamed alias, and the row's id is also what the served card carries in
	// its AgentInterface.tenant (§8.3.2), so a client that discovers the card
	// here sends the same value back on every A2A request.
	AgentCardAgentQueryParam = "agent_id"

	// ProtocolVersion is the A2A protocol version the advertised interface
	// exposes. §4.4.6 wants "the latest supported minor version per major
	// version", and specs/A2A-OPTION.md §2 binds crier to A2A v1.0.0's
	// JSON-RPC binding, so the value is "1.0" — not the spec's patch release.
	ProtocolVersion = "1.0"

	// ProtocolBindingJSONRPC is the only A2A protocol binding crier
	// implements (§8.3, and specs/A2A-OPTION.md §2: gRPC and HTTP+JSON are
	// declared MAY, not built). The value is the spec's own enum spelling for
	// the interface's `protocolBinding` field, which is an open-form string
	// whose core values are `JSONRPC`, `GRPC` and `HTTP+JSON`.
	ProtocolBindingJSONRPC = "JSONRPC"

	// JSONRPCBindingPath is the path the advertised interface points at.
	//
	// This is the one field of the card that is a forward declaration: the
	// AgentInterface.url must be the URL a client sends JSON-RPC methods to,
	// and the endpoint that answers them lands with INT-A2A-003. The choice is
	// recorded here (and in specs/A2A-OPTION.md §5.2) so that row serves THIS
	// path rather than inventing a second one; the card does not claim the
	// operations are mounted yet — the capability flags below are where a
	// client reads what is actually served (§4.4.3, and see Streaming).
	JSONRPCBindingPath = "/a2a"

	// CardMediaType is the A2A media type registration (§14.1.1) and what the
	// spec's own AgentCard example responses carry (§6.9 step 3).
	CardMediaType = "application/a2a+json"

	// CardCacheMaxAgeSeconds is the max-age the discovery route advertises
	// (§8.6.1 asks for "a max-age directive appropriate for the agent's
	// expected update frequency").
	//
	// The card is a projection of a registry row plus the serving process's
	// posture, so it changes when an operator PATCHes the row — minutes-to-days
	// apart — or when the binary is replaced (a restart re-reads it anyway).
	// One minute keeps a client from re-fetching on every interaction while
	// staying short enough that a capability change is visible almost
	// immediately; the ETag below is what makes the re-fetch cheap, and
	// conditional requests (§8.6.2) are the intended steady state.
	CardCacheMaxAgeSeconds = 60

	// docsPath is where the running server publishes this process's API
	// documentation (GET /docs, DF-CRIER-196) — the value the card reports as
	// its documentationUrl. It is a URL that exists on the SAME origin as the
	// interface advertised above, so a client that reached the card can reach
	// the documentation for the surface it describes.
	docsPath = "/docs"

	// jsonMediaType is the media type of crier's delivery and retrieval wire
	// (POST /agents/{id}/inbox takes a JSON envelope, GET .../inbox answers
	// one; the payload INSIDE the envelope is opaque). It is what the card
	// declares as its default input and output modes — media types, per §4.4.1
	// — because it is the only content type this bus actually exchanges today.
	jsonMediaType = "application/json"
)

// AgentCard is the A2A AgentCard (spec §4.4.1) projected from one registry row.
//
// Field presence follows §5.7: the fields the spec marks REQUIRED are always
// serialized — name, description, supportedInterfaces, version, capabilities,
// defaultInputModes, defaultOutputModes, skills — while the OPTIONAL ones are
// omitted entirely when crier has nothing to say (documentationUrl,
// securitySchemes, securityRequirements, and capabilities.extensions), never
// emitted as an empty placeholder. That is also what keeps the body stable
// enough to serve a content-derived ETag (§8.6.1).
//
// Absent by construction, not by accident: `provider` (crier has no provider
// identity to state — the service provider is the operator's, and a made-up
// organization string would be a claim nobody can check), `iconUrl` (no icon is
// served), and `signatures` (§8.4 — an AgentCardSignature is a JWS over an
// RFC 8785 canonicalized card, which this server does not compute; see
// specs/A2A-OPTION.md §7).
type AgentCard struct {
	Name                 string                    `json:"name"`
	Description          string                    `json:"description"`
	SupportedInterfaces  []AgentInterface          `json:"supportedInterfaces"`
	Version              string                    `json:"version"`
	DocumentationURL     string                    `json:"documentationUrl,omitempty"`
	Capabilities         AgentCapabilities         `json:"capabilities"`
	SecuritySchemes      map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []SecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                  `json:"defaultInputModes"`
	DefaultOutputModes   []string                  `json:"defaultOutputModes"`
	Skills               []AgentSkill              `json:"skills"`
}

// AgentInterface is one declared A2A interface (spec §4.4.6). The first — and
// today only — entry is the JSON-RPC binding.
type AgentInterface struct {
	// URL is where this interface is served. §8.3.1 requires it to be
	// accurate, and §4.4.6 wants an absolute URL, so it is built from the
	// origin the client actually reached this server on.
	URL string `json:"url"`
	// ProtocolBinding is ProtocolBindingJSONRPC.
	ProtocolBinding string `json:"protocolBinding"`
	// Tenant is the registry id. §4.4.6 defines it for exactly this shape —
	// several agents behind one A2A endpoint: a client MUST include it in the
	// `tenant` field of every request message it sends to this interface, which
	// is how a bus routes an A2A request to the right agent.
	Tenant string `json:"tenant,omitempty"`
	// ProtocolVersion is ProtocolVersion.
	ProtocolVersion string `json:"protocolVersion"`
}

// AgentCapabilities is the capability set (spec §4.4.3).
//
// Streaming and PushNotifications are ALWAYS serialized, even when false: the
// proto marks them optional, and §8.4.1's canonicalization example includes
// them when explicitly set — so a reader sees the server's stated answer rather
// than having to infer one from an omission.
type AgentCapabilities struct {
	// Streaming reports whether this agent supports streaming A2A responses.
	// It is TRUE whenever this card is served at all, and that is a
	// measurement: the card exists only while CR_A2A_ENABLED is set, and the
	// JSON-RPC binding that ships with it serves SendStreamingMessage as
	// Server-Sent Events (INT-A2A-003, specs/A2A-OPTION.md §5.4). §3.3.4 makes
	// this field load-bearing in the other direction too: a `false` here would
	// REQUIRE this server to answer SendStreamingMessage with
	// UnsupportedOperationError, which would be a false statement about a
	// surface it really does serve. The relay's own WebSocket subscribe is NOT
	// this — it is crier's protocol, not the A2A binding.
	Streaming bool `json:"streaming"`
	// PushNotifications reports what the agent actually has configured: it is
	// true exactly when the registry row carries a crier webhook (the
	// push-delivery channel this agent has — url, retries, timeout, batch),
	// which is the object the A2A push-notification config is defined to be a
	// view over (mapping table, specs/A2A-OPTION.md §3). An agent with no
	// webhook has no push channel, and says so.
	PushNotifications bool `json:"pushNotifications"`
	// Extensions are the A2A protocol extensions the agent declares (§4.6.1).
	// Omitted when empty, and empty today: crier declares no extension.
	Extensions []AgentExtension `json:"extensions,omitempty"`
}

// AgentExtension is a declared A2A protocol extension (spec §4.4.4).
type AgentExtension struct {
	URI         string         `json:"uri"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Params      map[string]any `json:"params,omitempty"`
}

// AgentSkill is one of the agent's skills (spec §4.4.5), projected from a
// crier capability tag. The projection is one-way and lossless in the direction
// that matters: crier's registry stays the source of truth, and the card invents
// no capability the row does not carry.
type AgentSkill struct {
	// ID is the capability tag verbatim, so the skill id is the identifier the
	// agent publishes on the bus.
	ID string `json:"id"`
	// Name is the tag too — crier's registry stores tags, not display names.
	Name string `json:"name"`
	// Description states what the skill is and where it comes from, because
	// the registry carries no per-skill prose to quote: an A2A client reading
	// this field gets a fact, not an invented capability claim.
	Description string `json:"description"`
	// Tags carries the tag as well: `tags` is the field §4.4.5 marks REQUIRED
	// for keyword matching, so the registry's own keyword is what a client
	// matches on.
	Tags []string `json:"tags"`
}

// SecurityScheme is the A2A SecurityScheme (spec §4.5.1): a discriminated
// union that serializes as an object carrying exactly one member, plus an
// optional description inside that member. Only the two scheme kinds crier
// really uses are modelled — an HTTP bearer and an apiKey-style header.
type SecurityScheme struct {
	HTTPAuth *HTTPAuthSecurityScheme `json:"httpAuthSecurityScheme,omitempty"`
	APIKey   *APIKeySecurityScheme   `json:"apiKeySecurityScheme,omitempty"`
}

// HTTPAuthSecurityScheme is spec §4.5.3 (RFC 7235-style HTTP authentication).
type HTTPAuthSecurityScheme struct {
	Description  string `json:"description,omitempty"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// APIKeySecurityScheme is spec §4.5.2 — the shape crier's OpenAPI spec already
// uses for its per-agent request signature (an `X-Agent-Sig` header).
type APIKeySecurityScheme struct {
	Description string `json:"description,omitempty"`
	Location    string `json:"location"`
	Name        string `json:"name"`
}

// SecurityRequirement is spec §4.5.1's map of scheme name to required scopes.
type SecurityRequirement struct {
	Schemes map[string]StringList `json:"schemes"`
}

// StringList is spec §4.x's scope list for one scheme. crier's bearer token has
// no scopes, so the list is emitted explicitly EMPTY rather than dropped: an
// absent `list` and an empty one are different readings of the same object, and
// "this scheme needs no scopes" is the one crier means.
type StringList struct {
	List []string `json:"list"`
}

// The security scheme names the card uses. They are crier's own OpenAPI
// component names (docs/openapi.yaml `components.securitySchemes`), so an
// operator who knows crier's spec reads the card without a translation table
// and a client library generated from that spec recognises both.
const (
	SchemeBearerAuth     = "bearerAuth"
	SchemeAgentSignature = "agentSignature"
	headerAgentSignature = "X-Agent-Sig"
	schemeLocationHeader = "header"
	schemeHTTPBearer     = "Bearer"
)

// CardRow is the registry-row evidence the projection needs, passed in as plain
// values so this package stays independent of internal/registry.
type CardRow struct {
	// AgentID is the registry id: the card's name, the interface's tenant, and
	// the value a client echoes back to route a request to this agent.
	AgentID string
	// Capabilities are the agent's advertised capability tags, in row order.
	Capabilities []string
	// PushConfigured reports whether the row carries a crier webhook.
	PushConfigured bool
}

// ServerInfo is the serving process's posture — the facts the card states about
// the SERVER rather than about the agent. They are passed in rather than read
// from the environment so the projection is a pure function of its inputs.
type ServerInfo struct {
	// Origin is the absolute origin the client reached this server on
	// (scheme://host), used for the interface URL and documentationUrl. A
	// trailing slash is tolerated and trimmed.
	Origin string
	// Version is the build identity serving the card (internal/buildinfo).
	// The card's `version` is the version of the A2A surface being described,
	// and the registry row carries no version of its own — the alternative
	// would be a second, invented source of truth.
	Version string
	// AuthTokenSet reports whether CR_AUTH_TOKEN is in force. When it is, the
	// card declares bearerAuth as a scheme AND as a requirement, because every
	// route of this server — this discovery route included — insists on it.
	AuthTokenSet bool
	// AgentSignatureEnforced reports whether CR_REQUIRE_AGENT_SIG is in force.
	// When it is, the card DECLARES the crier-native agentSignature scheme but
	// never demands it (see securitySchemes): a generic A2A client holds no
	// crier private key, so advertising it as a requirement would advertise a
	// scheme that client is guaranteed to fail.
	AgentSignatureEnforced bool
}

// BuildCard projects one opted-in registry row into an AgentCard.
//
// The caller decides whether the agent opted in (a2a.Config.OptedIn) and
// whether the server-side switch is on; this function assumes both halves are
// satisfied and never consults configuration of its own.
func BuildCard(row CardRow, srv ServerInfo) AgentCard {
	origin := strings.TrimSuffix(srv.Origin, "/")

	card := AgentCard{
		Name:        row.AgentID,
		Description: cardDescription(row),
		SupportedInterfaces: []AgentInterface{{
			URL:             origin + JSONRPCBindingPath,
			ProtocolBinding: ProtocolBindingJSONRPC,
			Tenant:          row.AgentID,
			ProtocolVersion: ProtocolVersion,
		}},
		Version:          srv.Version,
		DocumentationURL: origin + docsPath,
		Capabilities: AgentCapabilities{
			// TRUE: the JSON-RPC binding served alongside this card answers
			// SendStreamingMessage with text/event-stream (INT-A2A-003). A
			// `false` here would, by §3.3.4, require this server to refuse an
			// operation it serves.
			Streaming:         true,
			PushNotifications: row.PushConfigured,
		},
		DefaultInputModes:  []string{jsonMediaType},
		DefaultOutputModes: []string{jsonMediaType},
		Skills:             skillsFromCapabilities(row),
	}
	card.SecuritySchemes = srv.securitySchemes()
	card.SecurityRequirements = srv.securityRequirements()
	return card
}

// cardDescription states what the card describes and where it came from. It is
// deliberately about the ROW — the id, the capability tags — because that is
// the only agent metadata crier has: the registry stores no description field,
// and a sentence invented here would read as the agent's own claim.
func cardDescription(row CardRow) string {
	caps := "none registered"
	if len(row.Capabilities) > 0 {
		caps = strings.Join(dedupeStrings(row.Capabilities), ", ")
	}
	return fmt.Sprintf("crier agent %q — a registry row on this crier relay (agent-to-agent message bus), published for A2A clients because the agent opted in. Advertised capabilities: %s.", row.AgentID, caps)
}

// skillsFromCapabilities maps the row's capability tags onto AgentSkill[], in
// row order.
//
// Two properties are deliberate: the result is never nil (a JSON `null` where
// the spec wants an array is a different document — the field is REQUIRED), and
// a tag repeated in the row appears ONCE, because a skill id is an identifier:
// the same id twice is not a second skill, it is a row that lists one tag
// twice.
func skillsFromCapabilities(row CardRow) []AgentSkill {
	skills := make([]AgentSkill, 0, len(row.Capabilities))
	for _, tag := range dedupeStrings(row.Capabilities) {
		skills = append(skills, AgentSkill{
			ID:          tag,
			Name:        tag,
			Description: fmt.Sprintf("crier capability tag %q of agent %q (crier's registry stores capability tags, not skill prose).", tag, row.AgentID),
			Tags:        []string{tag},
		})
	}
	return skills
}

// dedupeStrings drops later duplicates while preserving first-seen order.
func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// securitySchemes is crier's real auth posture as securitySchemes (spec §4.5.1).
//
// Both schemes are declared only when they are actually in force: a card that
// listed a scheme the server does not enforce would be describing a different
// server. Nothing here changes what any route requires — the card describes the
// posture, it never sets it (specs/A2A-OPTION.md §1, §7).
func (srv ServerInfo) securitySchemes() map[string]SecurityScheme {
	schemes := map[string]SecurityScheme{}

	if srv.AuthTokenSet {
		schemes[SchemeBearerAuth] = SecurityScheme{HTTPAuth: &HTTPAuthSecurityScheme{
			Scheme: schemeHTTPBearer,
			Description: "crier's shared-secret bearer token (CR_AUTH_TOKEN). " +
				"Required on every route of this server, this Agent Card's own GET included: " +
				"fetch the card with Authorization: Bearer <token>.",
		}}
	}

	if srv.AgentSignatureEnforced {
		schemes[SchemeAgentSignature] = SecurityScheme{APIKey: &APIKeySecurityScheme{
			Location: schemeLocationHeader,
			Name:     headerAgentSignature,
			Description: "crier's native per-agent ed25519 request signature (CR_REQUIRE_AGENT_SIG), " +
				"computed over the agent id, a unix timestamp, the HTTP method and the raw path, and sent " +
				"with X-Agent-ID and X-Agent-Ts. It is NOT an A2A client credential: a generic A2A client " +
				"holds no crier agent key and cannot compute it, so it is declared here — because it is " +
				"enforced on crier's signed routes — but never listed in securityRequirements.",
		}}
	}

	if len(schemes) == 0 {
		// Neither posture is in force (no token, signatures off): crier
		// requires nothing, and the card says nothing rather than emitting an
		// empty object (§5.7 — an optional field with nothing to say is
		// omitted).
		return nil
	}
	return schemes
}

// securityRequirements is what a client must PRESENT (spec §4.4.1,
// SecurityRequirement §4.5.1) — the subset of the declared schemes that is not
// merely in force but demanded of an A2A client.
//
// bearerAuth qualifies: with CR_AUTH_TOKEN set, an A2A client gets 401 on every
// call without it. agentSignature deliberately does NOT, even when enforced:
// crier's ed25519 trio is bound to an agent's own private key, so demanding it
// of a generic A2A client would advertise a requirement that client cannot
// satisfy. The honest form is what this returns — declare the scheme, do not
// demand it.
func (srv ServerInfo) securityRequirements() []SecurityRequirement {
	if !srv.AuthTokenSet {
		return nil
	}
	return []SecurityRequirement{{
		Schemes: map[string]StringList{
			SchemeBearerAuth: {List: []string{}},
		},
	}}
}

// CardETag is the strong entity tag served with the card (spec §8.6.1: an ETag
// "derived from the Agent Card's version field or a hash of the card content").
//
// The content hash is the choice: the version field moves only when the BINARY
// moves, while the card also changes when the row behind it changes (a new
// capability, a webhook added, or the auth posture flipping), and those are the
// updates a client most needs to see. The tag is the SHA-256 of the exact bytes
// served, so any change to the projection changes the tag.
func CardETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
