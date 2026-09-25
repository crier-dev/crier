// routehint.go — CR-FEAT-031: a near-miss 404 names the shape it nearly was.
//
// The external review (DISPATCH · CRI-001, Carter, 2026-09-25) lost its first
// few minutes to the most ordinary question a new client asks: "what is the
// correct path?". It tried `GET /relay/subscribe?topic=…` and got a 404; the
// real shape is `/relay/subscribe/{topic}`. DF-CRIER-213 put that 404 in the
// repo's JSON envelope, which made it DECODABLE but still said only
// "not found" — it never said which shape the server does serve. This file
// closes that gap and changes nothing else: when the request path is exactly
// one segment away from a route the router really serves, the 404 body is
// EXTENDED (never replaced) with a prose `hint` naming the correct form and a
// machine-readable `did_you_mean` holding the route template.
//
// Both halves of the answer come from sources that cannot drift from what the
// server does:
//
//   - the templates and methods come from the LIVE router (mux's Walk), so a
//     hint can never name a route this server does not serve;
//   - the parameter names come from the EMBEDDED OpenAPI spec — the same bytes
//     GET /openapi.json serves — so a hint can never name a parameter the
//     documents do not declare.
//
// What it can be wrong about, stated rather than left to be discovered:
//
//   - it fires only on a ONE-segment miss (a path that matches a registered
//     template except for one trailing segment, in either direction). Two
//     segments away is still a plain "not found", and so is every path that
//     looks like nothing the router serves;
//   - `did_you_mean` is the TEMPLATE as registered — a shape, not a request
//     you can paste. The concrete form, where one is derivable, is in `hint`;
//   - a query parameter declared only through a `$ref` to a components/parameters
//     entry is not named here (the walk reads inline `in: query` entries), and
//     a path the spec does not describe gets the template hint with no
//     invented parameter names;
//   - it is not authorization-aware, and does not need to be: route names and
//     parameter names are already public — /docs and /openapi.json are
//     auth-exempt (internal/middleware/auth.go) — so the hint cannot disclose
//     anything a token-less caller could not read;
//   - it reads no body and no header: the shape question is answered from the
//     request line alone, so a hint can never depend on a client's payload.
package main

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v3"
)

// notFoundBody is the router's 404 body: the repo's {"error": …} envelope
// EXTENDED — never replaced — with the shape hint when the request was a near
// miss. A path with no near miss has empty extras, and `omitempty` makes that
// marshal to the byte-identical body DF-CRIER-213 shipped
// (`{"error":"not found"}\n`), so nothing about the plain answer moved.
//
// The struct (rather than a map) is deliberate: it keeps the field order on the
// wire, so `error` is still the first key a human reads in a raw body.
type notFoundBody struct {
	Error      string `json:"error"`
	Hint       string `json:"hint,omitempty"`
	DidYouMean string `json:"did_you_mean,omitempty"`
}

// routeShapeHint is the answer to "this path is one segment off — off WHAT?".
type routeShapeHint struct {
	// DidYouMean is the route template as the router registered it, e.g.
	// `/relay/subscribe/{topic}`. It is always a shape this server serves, and
	// never a path this code assembled.
	DidYouMean string
	// Message names the correction in prose, including a concrete corrected
	// request where one is derivable from the request itself.
	Message string
}

// routeShape is one path template the router actually serves, plus the
// request-level inputs the embedded spec documents for it.
type routeShape struct {
	template string
	segs     []string
	// methods as registered; empty means the route matches every method.
	methods []string
	// query holds the documented `in: query` parameter names, in spec order.
	query []string
	// body holds the documented required JSON body property names, in spec
	// order.
	body []string
}

// routeShapes is the server's route table, indexed for near-miss questions.
type routeShapes struct {
	shapes []routeShape
}

// newRouteShapes indexes the live router. It must be called once the router is
// fully populated — see the shapesFor closure in registerRouterFallbacks
// (fallback.go), which defers it to the first unmatched request for exactly that
// reason.
func newRouteShapes(r *mux.Router) routeShapes {
	inputs := specRouteInputs()
	var shapes []routeShape
	// Walk's error is only non-nil when the walk function returns one; this one
	// never does, and a route it cannot read contributes no shape rather than
	// failing the request (a hint is optional — a 404 without one is still a
	// correct 404).
	_ = r.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tpl, err := route.GetPathTemplate()
		if err != nil {
			return nil // a route with no path contributes no path shape
		}
		methods, _ := route.GetMethods() // error == no method matcher == any method
		in := inputs.forRoute(tpl, methods)
		shapes = append(shapes, routeShape{
			template: tpl,
			segs:     pathSegments(tpl),
			methods:  methods,
			query:    in.query,
			body:     in.body,
		})
		return nil
	})
	return routeShapes{shapes: shapes}
}

// hintFor answers the near-miss question for one request. It returns false when
// the path is not one segment away from a registered route — that is the
// overwhelmingly common case, and the caller then writes the plain envelope.
func (rs routeShapes) hintFor(req *http.Request) (routeShapeHint, bool) {
	reqSegs := pathSegments(req.URL.Path)
	if len(reqSegs) == 0 {
		return routeShapeHint{}, false
	}
	queries := req.URL.Query()

	var (
		chosen    routeShape
		direction missDirection
		extra     string
		score     = -1
	)
	for _, sh := range rs.shapes {
		dir, ex, ok := sh.against(reqSegs)
		if !ok {
			continue
		}
		// Ranking, in order of what makes a candidate the one the caller meant:
		//
		//  1. it serves the request's method (a route registered for another
		//     method is still worth naming — the miss is the path, not the
		//     verb — but it must not outrank the route that would have
		//     answered this request). A route with no method matcher serves
		//     every method, so it ranks here too;
		//  2. the request NAMES the thing it is missing from the path, either
		//     as the query parameter key that equals the missing placeholder
		//     (`/relay/subscribe` + `?topic=…` against
		//     `/relay/subscribe/{topic}`) or as the `name=value` segment
		//     whose name the spec documents as a query parameter on that route
		//     (`…/inbox/wait_seconds=30`).
		//
		// Ties keep the first template in registration order (Walk is
		// registration order), so the same request always gets the same hint.
		//
		// The method term outranks the name term because the two routes that
		// share a template — POST and GET on /agents/{id}/inbox — differ in
		// exactly that: the wrong pick would name the POST route's JSON body
		// to a GET.
		s := 1
		if sh.servesMethod(req.Method) {
			s += 4
		}
		if direction == missRouteHasMoreSegment {
			if queries.Has(placeholderName(sh.segs[len(sh.segs)-1])) {
				s += 2
			}
		} else if name, _, ok := splitAssignment(ex); ok && slices.Contains(sh.query, name) {
			s += 2
		}
		if s > score {
			chosen, direction, extra, score = sh, dir, ex, s
		}
	}
	if score < 0 {
		return routeShapeHint{}, false
	}

	path := "/" + strings.Join(reqSegs, "/")
	note := methodNote(req, chosen)
	if direction == missRouteHasMoreSegment {
		return routeShapeHint{
			DidYouMean: chosen.template,
			Message:    missingSegmentMessage(req, path, chosen, queries) + note,
		}, true
	}
	return routeShapeHint{
		DidYouMean: chosen.template,
		Message:    extraSegmentMessage(req, path, chosen, extra) + note,
	}, true
}

// missDirection says which side of a near miss carries the extra segment.
type missDirection int

const (
	// missRouteHasMoreSegment: the route is one segment longer than the
	// request and that last segment is a placeholder — the route needs a path
	// segment the request did not supply. This is the review's case:
	// `/relay/subscribe` for `/relay/subscribe/{topic}`.
	missRouteHasMoreSegment missDirection = iota
	// missRequestHasExtraSegment: the request carries one segment more than
	// the route — a value that belongs in the query string, in the JSON body,
	// or nowhere at all.
	missRequestHasExtraSegment
)

// against classifies one request path against one route template.
//
// The two shapes it accepts are exactly one trailing segment apart. In the
// first case that segment is the template's LAST placeholder (a literal last
// segment means the request is missing a fixed part of the path, e.g.
// `/agents/{id}/inbox/ack`, and is not a shape question); in the second case
// every template segment matched and the request supplied one more.
func (sh routeShape) against(reqSegs []string) (missDirection, string, bool) {
	switch {
	case len(sh.segs) > 0 && len(reqSegs) == len(sh.segs)-1 && isPlaceholder(sh.segs[len(sh.segs)-1]):
		if segmentsMatch(sh.segs[:len(reqSegs)], reqSegs) {
			return missRouteHasMoreSegment, "", true
		}
	case len(reqSegs) == len(sh.segs)+1:
		if segmentsMatch(sh.segs, reqSegs[:len(sh.segs)]) {
			return missRequestHasExtraSegment, reqSegs[len(sh.segs)], true
		}
	}
	return 0, "", false
}

// servesMethod reports whether the route would answer this method: a route with
// no method matcher answers every method, which is how the WebSocket routes are
// registered.
func (sh routeShape) servesMethod(method string) bool {
	return len(sh.methods) == 0 || slices.Contains(sh.methods, method)
}

// missingSegmentMessage names the segment the route wants, and — when the
// request itself says what the value is — shows the request that would have
// worked. The value is taken from the query string because that is where a
// wrong-shaped first attempt puts it: the query key matching the placeholder
// name wins, else the single query value the request carried.
func missingSegmentMessage(req *http.Request, path string, sh routeShape, queries url.Values) string {
	name := placeholderName(sh.segs[len(sh.segs)-1])
	key, val, ok := queryValueFor(queries, name)

	if !ok {
		return fmt.Sprintf("%s %s is not a route; %s is one path segment longer — the %s belongs in the PATH, not in the query string. Append the %s as the last path segment: %s %s/%s",
			req.Method, path, sh.template, name, name, req.Method, path, sh.segs[len(sh.segs)-1])
	}
	// The value is percent-encoded for the PATH it is being moved into, and only
	// then sanitized: a query value may contain "/" or a space (pasting it raw
	// would suggest a path that is itself a different shape), and escaping first
	// is what keeps the suggestion the value the client actually sent.
	return fmt.Sprintf("%s %s is not a route; %s is, and the %s belongs in the PATH, not in the query string — you sent ?%s=%s. Try %s %s/%s",
		req.Method, path, sh.template, name, key, echo(val), req.Method, path, echo(url.PathEscape(val)))
}

// extraSegmentMessage names the route that ends before the stray segment, and
// what that segment should have been: a documented `name=value` query parameter
// (named exactly), a documented query parameter in general, or the JSON body.
// When the spec documents none of those, it still says the segment is not part
// of the route — which is the whole correction for `/health/foo`.
func extraSegmentMessage(req *http.Request, path string, sh routeShape, extra string) string {
	segs := pathSegments(path)
	corrected := "/" + strings.Join(segs[:len(segs)-1], "/")

	if name, value, ok := splitAssignment(extra); ok && slices.Contains(sh.query, name) {
		return fmt.Sprintf("%s %s is not a route; %s takes no trailing path segment — %s is a QUERY parameter, not a path segment. Try %s %s?%s=%s",
			req.Method, path, sh.template, name, req.Method, corrected, name, echo(url.QueryEscape(value)))
	}

	msg := fmt.Sprintf("%s %s is not a route; %s takes no trailing path segment (%q is not part of it). Try %s %s",
		req.Method, path, sh.template, echo(extra), req.Method, corrected)
	switch {
	case len(sh.query) > 0:
		msg += fmt.Sprintf("; its request-level options are query parameters (?%s)", strings.Join(sh.query, ", ?"))
	case len(sh.body) > 0:
		msg += fmt.Sprintf("; the payload goes in the JSON body (%s)", strings.Join(sh.body, ", "))
	}
	return msg
}

// methodNote adds the one thing the path hint cannot imply when the route it
// names does not answer this verb: which verb it does answer. Without it,
// `GET /relay/publish/news` would be told to try `POST /relay/publish` in a
// sentence that never says the method changes.
func methodNote(req *http.Request, sh routeShape) string {
	if sh.servesMethod(req.Method) {
		return ""
	}
	return fmt.Sprintf(". That path serves %s, not %s.", strings.Join(sh.methods, "/"), req.Method)
}

// queryValueFor picks the value to show in a corrected path: the query key that
// matches the placeholder's own name first (the request said "topic", the route
// wants a path segment called topic), else the only value-bearing key the
// request carried (a first attempt that used one query parameter meant that
// one), else nothing — in which case no value is invented.
func queryValueFor(queries url.Values, name string) (key, val string, ok bool) {
	if vs := queries[name]; len(vs) > 0 && vs[0] != "" {
		return name, vs[0], true
	}
	if len(queries) != 1 {
		return "", "", false
	}
	for k, vs := range queries {
		if len(vs) > 0 && vs[0] != "" {
			return k, vs[0], true
		}
	}
	return "", "", false
}

// routeHintEchoMax bounds every request-supplied string that is echoed into a
// hint. The request line is attacker-controlled and unbounded (a path segment
// can be kilobytes), and a hint is a response body, so the echo is bounded and
// sanitized rather than reflected.
const routeHintEchoMax = 64

// echo makes a request-supplied string safe to place in a hint: control
// characters (a %0A in a query value decodes to a real newline) become "?", an
// invalid byte becomes "?", and anything past routeHintEchoMax is cut on a rune
// boundary with a trailing ellipsis so a truncated value cannot read as the
// whole value.
//
// "<", ">" and "&" are sanitized too. They are legal in a query value, and Go's
// JSON encoder escapes them as \u003c/\u003e/\u0026 — valid JSON that decodes
// back to the original, but a hint exists to be read in a raw body, so the
// echoed value must not be the one thing in this API that turns into escapes.
func echo(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= routeHintEchoMax {
			b.WriteString("…")
			break
		}
		if r < 0x20 || r == 0x7f || r == utf8.RuneError || r == '<' || r == '>' || r == '&' {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pathSegments splits a URL path (or a route template) into its non-empty
// segments: "/relay/subscribe/{topic}" and "/relay//subscribe/x/" both split to
// three and two segments respectively, so an empty segment is never a near-miss
// difference of its own.
func pathSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// segmentsMatch reports whether every template segment matches the request
// segment in the same position: a placeholder matches anything, a literal must
// be equal.
func segmentsMatch(tpl, req []string) bool {
	if len(tpl) != len(req) {
		return false
	}
	for i := range tpl {
		if !isPlaceholder(tpl[i]) && tpl[i] != req[i] {
			return false
		}
	}
	return true
}

// isPlaceholder reports whether a template segment is a mux variable
// (`{topic}`), including the constrained form (`{id:[0-9]+}`).
func isPlaceholder(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

// placeholderName is the variable's own name — the word the hint uses to say
// what is missing ("the topic belongs in the PATH") — from `{topic}` or the
// constrained `{id:[0-9]+}`.
func placeholderName(seg string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	return name
}

// splitAssignment splits a path segment carrying a query parameter's
// name=value form (`wait_seconds=30`) — what a path segment looks like when
// someone puts a query parameter where a path segment belongs.
func splitAssignment(seg string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(seg, "=")
	if !ok || name == "" {
		return "", "", false
	}
	return name, value, true
}

// ---------------------------------------------------------------------------
// The embedded spec's request-level inputs.
// ---------------------------------------------------------------------------

// routeInputs is what the spec documents for one path: the query parameter names
// and the required JSON body property names.
type routeInputs struct {
	query []string
	body  []string
}

// merge folds b into a, keeping a's order and skipping names already present.
func (a routeInputs) merge(b routeInputs) routeInputs {
	for _, q := range b.query {
		if !slices.Contains(a.query, q) {
			a.query = append(a.query, q)
		}
	}
	for _, f := range b.body {
		if !slices.Contains(a.body, f) {
			a.body = append(a.body, f)
		}
	}
	return a
}

// specInputSet is the spec's inputs indexed by path and method.
type specInputSet struct {
	byPath map[string]map[string]routeInputs
}

// specMethodOrder is the fixed order methods are merged in when a route carries
// no method constraint (or more than one), so a hint's parameter list is
// deterministic rather than map-iteration dependent.
var specMethodOrder = []string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodDelete, http.MethodHead, http.MethodOptions, http.MethodTrace,
}

// forRoute returns the inputs documented for a path, restricted to the route's
// own methods when it has any, else unioned across every method the spec
// declares for that path (a route with no method matcher answers all of them).
// An unknown path yields empty inputs, and the hint then names the template
// alone rather than a parameter nobody documented.
func (s specInputSet) forRoute(path string, methods []string) routeInputs {
	byMethod := s.byPath[path]
	if len(byMethod) == 0 {
		return routeInputs{}
	}
	var out routeInputs
	if len(methods) == 0 {
		for _, m := range specMethodOrder {
			out = out.merge(byMethod[m])
		}
		return out
	}
	for _, m := range methods {
		out = out.merge(byMethod[strings.ToUpper(m)])
	}
	return out
}

// specRouteInputs reads the embedded OpenAPI spec — the same bytes
// GET /openapi.json serves — for each path's query parameters and required JSON
// body properties.
//
// A parse failure here cannot be reached in practice (openapi.go's init panics
// on an unparseable embedded spec, and init runs first), but it must not take
// the server down with it if that ever changes: this returns an empty set, and
// every hint degrades to naming the template alone.
func specRouteInputs() specInputSet {
	out := specInputSet{byPath: map[string]map[string]routeInputs{}}

	var root yaml.Node
	if err := yaml.Unmarshal(openapiYAML, &root); err != nil || len(root.Content) == 0 {
		return out
	}
	paths := mappingValue(root.Content[0], "paths")
	if paths == nil || paths.Kind != yaml.MappingNode {
		return out
	}

	for i := 0; i+1 < len(paths.Content); i += 2 {
		path := paths.Content[i].Value
		item := paths.Content[i+1]
		if item.Kind != yaml.MappingNode {
			continue
		}
		// Path-item-level parameters apply to every operation on the path.
		shared := routeInputs{query: queryParameters(item)}
		for j := 0; j+1 < len(item.Content); j += 2 {
			method := strings.ToUpper(item.Content[j].Value)
			op := item.Content[j+1]
			if !httpMethods[method] || op.Kind != yaml.MappingNode {
				continue
			}
			in := shared.merge(routeInputs{
				query: queryParameters(op),
				body:  requiredBodyFields(op),
			})
			if out.byPath[path] == nil {
				out.byPath[path] = map[string]routeInputs{}
			}
			out.byPath[path][method] = in
		}
	}
	return out
}

// queryParameters returns the names of an operation's (or path item's) inline
// `in: query` parameters, in spec order. A `$ref` entry is skipped rather than
// resolved — see the file comment's stated limit.
func queryParameters(node *yaml.Node) []string {
	params := mappingValue(node, "parameters")
	if params == nil || params.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, entry := range params.Content {
		if in := mappingValue(entry, "in"); in == nil || in.Value != "query" {
			continue
		}
		if name := mappingValue(entry, "name"); name != nil && name.Value != "" {
			out = append(out, name.Value)
		}
	}
	return out
}

// requiredBodyFields returns the required properties of an operation's
// application/json request body, in spec order. A `$ref` schema is skipped
// rather than resolved, the same stated limit as a `$ref` parameter.
func requiredBodyFields(op *yaml.Node) []string {
	schema := mappingValue(mappingValue(mappingValue(mappingValue(op, "requestBody"), "content"), "application/json"), "schema")
	required := mappingValue(schema, "required")
	if required == nil || required.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, entry := range required.Content {
		if entry.Value != "" {
			out = append(out, entry.Value)
		}
	}
	return out
}
