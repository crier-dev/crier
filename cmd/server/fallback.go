// fallback.go — the router's own 404 and 405 answers, served in the repo's
// JSON envelope instead of the stdlib/router defaults (DF-CRIER-213).
package main

import (
	"net/http"
	"strings"
	"sync"

	"github.com/crier-dev/crier/internal/httperr"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/gorilla/mux"
)

// The two messages written by the router's JSON fallbacks. They are prose
// about the HTTP layer, not about an application resource, so they carry no
// id or detail: the same wording the defaults implied, in the repo's
// {"error": …} envelope (internal/httperr).
const (
	notFoundMessage         = "not found"
	methodNotAllowedMessage = "method not allowed"
)

// probeMethods is the method set a request may match a route with, in a
// stable order so a derived Allow header value is deterministic. HEAD and
// OPTIONS are included even though no route here registers them: the Allow
// list is derived from the router, so a future route using either is picked
// up without touching this table.
var probeMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
}

// registerRouterFallbacks installs the JSON 404 and 405 answers on r, each
// wrapped so an unmatched request is correlated and logged exactly like a
// matched one: RequestID outermost (the X-Request-Id echo has to be in
// place before any status line is written), Logging innermost (it records
// the status the fallback actually wrote — the 404/405 itself).
//
// Why this is needed: gorilla/mux sends a request no registered route
// matches to Router.NotFoundHandler, defaulting to net/http's
// http.NotFoundHandler (body "404 page not found\n" served as text/plain;
// charset=utf-8), and a request whose PATH matches but whose METHOD does not
// to Router.MethodNotAllowedHandler, whose default writes WriteHeader(405)
// with no body and no Content-Type at all. Both answers are undecodable for
// a strict JSON client on an API whose 200s, 400s and 401s are all
// {"error": …}.
//
// The r.Use(…) chain in run() cannot close that gap: gorilla builds that
// chain inside Router.Match, only for a handler a route actually matched, so
// both defaults also ran outside middleware.RequestID and
// middleware.Logging — a mistyped path came back with no X-Request-Id and
// left no access-log line. Hence the explicit wrapping here.
//
// Deliberately NOT wrapped in middleware.Auth: an unknown path is not an
// authentication decision, and answering 401 there would misreport "no such
// route" as "bad credentials" to every probe (and change today's behaviour
// beyond the body and headers). Deliberately NOT wrapped in
// middleware.Recovery either: it exists to turn a panic raised inside a real
// handler into a JSON 500, and these two fallbacks are constant writers that
// cannot panic — wrapping them would widen the chain without covering
// anything.
//
// The 404 body is EXTENDED, never replaced, when the request path is one
// segment away from a route the router serves (CR-FEAT-031, routehint.go): it
// names the correct shape so a first attempt cannot be lost to guessing. A path
// with no near miss keeps the byte-identical body of DF-CRIER-213, and the 405
// is untouched — its Allow header already names the shape.
func registerRouterFallbacks(r *mux.Router) {
	// The route table is indexed on the FIRST 404 rather than here: run()
	// installs these fallbacks early, before most routes exist, and routes are
	// all registered before the listener is bound, so the first request always
	// sees the complete table. sync.Once keeps it to one Walk per server, and
	// the closure is per-router, so two servers in one process (the test
	// harness boots several) never share an index.
	var (
		shapesOnce sync.Once
		shapes     routeShapes
	)
	shapesFor := func() routeShapes {
		shapesOnce.Do(func() { shapes = newRouteShapes(r) })
		return shapes
	}

	r.NotFoundHandler = middleware.RequestID(middleware.Logging(
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if hint, ok := shapesFor().hintFor(req); ok {
				httperr.WriteJSON(w, http.StatusNotFound, notFoundBody{
					Error:      notFoundMessage,
					Hint:       hint.Message,
					DidYouMean: hint.DidYouMean,
				})
				return
			}
			httperr.WriteJSONError(w, http.StatusNotFound, notFoundMessage)
		}),
	))

	r.MethodNotAllowedHandler = middleware.RequestID(middleware.Logging(
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// Set before the status line: Allow is only valid on the 405 it
			// describes.
			if allow := allowedMethods(r, req); len(allow) > 0 {
				w.Header().Set("Allow", strings.Join(allow, ", "))
			}
			httperr.WriteJSONError(w, http.StatusMethodNotAllowed, methodNotAllowedMessage)
		}),
	))
}

// allowedMethods returns the methods r serves for the requested PATH, in
// probeMethods order, by probing the router's own match table once per
// method — the same question the router answered when it decided this
// request was a 405.
//
// A probe writes only into the throwaway RouteMatch it is given: in gorilla
// v1.8.1 Route.Match mutates that match (MatchErr, Route, Handler, Vars) and
// nothing on the route or the router, and every route here is registered
// before the server starts serving. Running a probe from inside a handler is
// therefore safe, including under concurrent requests.
//
// Classifying a probe: Router.Match lets a full match through with MatchErr
// nil, but its own 404 branch sets ErrNotFound and its 405 branch leaves
// ErrMethodMismatch in place — so "this method is served at this path" is
// exactly matched && MatchErr == nil. An empty result means the path itself
// is unknown; the caller then leaves Allow unset rather than fabricating a
// value.
func allowedMethods(r *mux.Router, req *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		probe := req.Clone(req.Context())
		probe.Method = method

		var match mux.RouteMatch
		if r.Match(probe, &match) && match.MatchErr == nil {
			allowed = append(allowed, method)
		}
	}
	return allowed
}
