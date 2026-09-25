// a2acard.go — the A2A Agent Card discovery route (INT-A2A-002,
// specs/A2A-OPTION.md §5.2).
//
// This is the FIRST route the A2A option registers, and it is OPT-IN twice over:
// it exists only while CR_A2A_ENABLED is set (run() registers it conditionally —
// §4.1), and it serves a card only for a registry row that opted in through its
// own `a2a` block (§4.2). An agent that did not opt in is not advertised: the
// route answers 404 for it, never an empty card.
//
// What makes this a projection rather than a second registry: the card is built
// from the row the request names, on every request, by internal/a2a.BuildCard —
// there is no card store, no cache and no copy that could disagree with
// GET /agents/{id}.
//
// It adds NO requirement to any existing route and changes none of them: the
// route is new, it inherits the middleware chain unchanged (so with
// CR_AUTH_TOKEN set it needs the same Bearer header as everything else, which
// is why the served card declares bearerAuth as a requirement), and the
// auth-exempt list in internal/middleware/auth.go is untouched.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/httperr"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/gorilla/mux"
)

// a2aCardOptions is the serving process's posture, captured at boot: the card
// states the auth posture that is actually in force, and these are the inputs
// that decide it. They are read by the handler (not by internal/a2a), so the
// projection stays a pure function of the evidence it is handed.
type a2aCardOptions struct {
	// port is the fallback origin port when a request arrives without a Host
	// header (an HTTP/1.0 client, or a test dialing the listener directly).
	port int
	// authTokenSet reports CR_AUTH_TOKEN is in force (cfg.AuthToken != "").
	authTokenSet bool
	// agentSignatureEnforced reports CR_REQUIRE_AGENT_SIG is in force.
	agentSignatureEnforced bool
}

// registerA2ACardRoute registers GET /.well-known/agent-card.json on r. It is
// called from run() ONLY when cfg.A2AEnabled is true, so with the switch unset
// the path is not registered at all and answers exactly what it answered before
// the A2A option existed — the router's JSON 404.
func registerA2ACardRoute(r *mux.Router, store registry.Store, opts a2aCardOptions) {
	r.HandleFunc(a2a.AgentCardPath, newA2ACardHandler(store, opts)).Methods(http.MethodGet)
}

// newA2ACardHandler returns the discovery handler: it resolves the registry row
// named by ?agent_id, refuses everything that is not an opted-in agent, and
// serves the projection with the caching headers §8.6.1 asks for.
func newA2ACardHandler(store registry.Store, opts a2aCardOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimSpace(r.URL.Query().Get(a2a.AgentCardAgentQueryParam))
		if agentID == "" {
			// A missing selector is a malformed request, not a missing
			// resource: this bus hosts many agents on one origin, so the
			// discovery path cannot name one on its own. 400 (not 404) also
			// keeps "the route is not registered" — which is a plain 404 —
			// distinguishable from "the route is registered and you did not
			// say which agent".
			httperr.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf(
				"missing required query parameter %s: an Agent Card is per agent — GET %s?%s=<registry id>",
				a2a.AgentCardAgentQueryParam, a2a.AgentCardPath, a2a.AgentCardAgentQueryParam))
			return
		}

		agent, err := store.Get(agentID)
		if err != nil {
			// A store that cannot answer is not the same fact as an agent that
			// does not exist: reporting a storage failure as 404 would tell the
			// caller the agent is absent when the truth is that nobody looked.
			if !errors.Is(err, registry.ErrAgentNotFound) {
				slog.Error("A2A Agent Card: registry lookup failed", "agent_id", agentID, "error", err)
				httperr.WriteJSONError(w, http.StatusInternalServerError, "registry storage unavailable")
				return
			}
			writeAgentCardRefusal(w, agentID)
			return
		}
		if !agent.A2A.OptedIn() {
			// The per-agent half of the gate. Refused with the same answer an
			// unknown id gets: an A2A client gains nothing from this server
			// telling it which ids exist but did not opt in, and /agents/{id}
			// (authenticated) is where that question belongs.
			writeAgentCardRefusal(w, agentID)
			return
		}

		card := a2a.BuildCard(a2a.CardRow{
			AgentID:        agent.ID,
			Capabilities:   agent.Capabilities,
			PushConfigured: agent.Webhook != nil,
		}, a2a.ServerInfo{
			Origin:                 cardOrigin(r, opts.port),
			Version:                buildinfo.String(),
			AuthTokenSet:           opts.authTokenSet,
			AgentSignatureEnforced: opts.agentSignatureEnforced,
		})

		// Encode into a buffer before touching the response: the body is
		// hashed for the ETag, so a partial write would serve bytes whose tag
		// describes something else.
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(card); err != nil {
			slog.Error("A2A Agent Card: encode failed", "agent_id", agent.ID, "error", err)
			httperr.WriteJSONError(w, http.StatusInternalServerError, "encode Agent Card")
			return
		}
		body := buf.Bytes()

		h := w.Header()
		// The caching contract (§8.6.1). `private` and not `public`: the card
		// is served behind crier's own auth, so a shared cache must never hand
		// one client's authorized response to another.
		h.Set("Cache-Control", fmt.Sprintf("private, max-age=%d", a2a.CardCacheMaxAgeSeconds))
		h.Set("ETag", a2a.CardETag(body))

		if ifNoneMatch(r.Header.Get("If-None-Match"), h.Get("ETag")) {
			// §8.6.2: the steady state is a client asking "still the same?".
			// A 304 carries the refreshed validators and no body.
			w.WriteHeader(http.StatusNotModified)
			return
		}

		h.Set("Content-Type", a2a.CardMediaType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// writeAgentCardRefusal is the single answer for "no card here": an id that is
// not a registry row, and a row that never opted in. The message names both
// causes and the fix, because the ids that reach this path are exactly the ones
// an integrator is iterating over.
func writeAgentCardRefusal(w http.ResponseWriter, agentID string) {
	httperr.WriteJSONError(w, http.StatusNotFound, fmt.Sprintf(
		"no A2A Agent Card for agent %q: the id is not a registry row on this relay, or the agent has not opted in (set \"a2a\":{\"enabled\":true} on POST /agents or PATCH /agents/{id})",
		agentID))
}

// cardOrigin is the absolute origin the client reached this server on, which is
// what the card's interface URL and documentationUrl are built from — §4.4.6
// wants an absolute URL, and the URL the client just used is the one URL the
// server can state without guessing a public hostname it cannot know (crier
// sits behind whatever proxy an operator puts in front of it).
//
// A request without a Host header falls back to the port this process bound, so
// the card still carries a well-formed absolute URL rather than a relative one.
func cardOrigin(r *http.Request, port int) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = fmt.Sprintf("localhost:%d", port)
	}
	return scheme + "://" + host
}

// ifNoneMatch reports whether the request's If-None-Match header covers etag.
//
// RFC 9110 §13.1.2: the value is a comma-separated list, `*` matches any current
// representation, and the comparison for GET is WEAK — a client echoing back
// `W/"…"` must be treated as a match even though this server only ever emits
// strong tags.
func ifNoneMatch(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
