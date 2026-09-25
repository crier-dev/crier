// crfeat031_test.go — CR-FEAT-031 acceptance evidence: a wrong-shaped request
// names the right shape.
//
// The external hands-on review (DISPATCH · CRI-001, Carter, delivered
// 2026-09-25 via Bane) lost its first minutes to the oldest discoverability
// question there is — what IS the path? It tried
// `GET /relay/subscribe?topic=…` and got a 404; the real shape is
// `/relay/subscribe/{topic}`.
//
// Measured here on a binary built from the pre-fix HEAD of this worktree
// (13632c3), on a scratch port, before any of the code below existed:
//
//	GET /relay/subscribe?topic=news   404, Content-Type: application/json,
//	                                  X-Request-Id echoed,
//	                                  body {"error":"not found"}
//	GET /nope                         404, the SAME body — the two are
//	                                  indistinguishable to the caller
//	DELETE /health                    405, Allow: GET, {"error":"method not allowed"}
//
// So DF-CRIER-213 had already made the answer decodable; it still never said
// which shape the server serves. What is pinned below is the post-fix
// contract, asserted against the REAL router via startTestServer (the
// cmd/server harness: auth off, in-memory registry, free port, run(nil)),
// so what is measured is the wire contract of the running server:
//
//   - a request one path segment away from a registered route gets a body that
//     NAMES that route (and, where the request itself says what the value is,
//     the request that would have worked);
//   - `did_you_mean` is the route template, and substituting a value for its
//     placeholders reaches a route the server really serves — the hint cannot
//     name a shape that is itself a 404;
//   - a path that is NOT a near miss keeps the byte-identical DF-CRIER-213
//     body, and the 405 is untouched (its Allow header already names the
//     shape);
//   - the echoed request text is bounded and sanitized: a hint is a response
//     body, and the request line is caller-controlled.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// hintBody is a decoded hint 404: the repo's envelope's `error`, plus the two
// fields a near miss adds.
type hintBody struct {
	Error      string `json:"error"`
	Hint       string `json:"hint"`
	DidYouMean string `json:"did_you_mean"`
}

// probeHint issues one request and asserts the full response contract of a hint
// 404 — status, the exact JSON Content-Type, the nosniff hardening header, a
// non-empty correlation id — then returns the decoded body (for the hint's own
// assertions) together with the raw response (for the byte-level ones).
func probeHint(t *testing.T, client *http.Client, method, url string) (hintBody, wireResponse) {
	t.Helper()

	got := probeWire(t, client, method, url, nil)
	name := method + " " + url
	if got.status != http.StatusNotFound {
		t.Fatalf("%s: status %d, want 404 (a near miss is still a 404 — the hint names a shape, it does not redirect)", name, got.status)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type %q, want application/json", name, ct)
	}
	if got.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options %q, want nosniff", name, got.header.Get("X-Content-Type-Options"))
	}
	if id := got.header.Get("X-Request-Id"); strings.TrimSpace(id) == "" {
		t.Errorf("%s: X-Request-Id missing — the hint 404 ran outside middleware.RequestID", name)
	}

	var body hintBody
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("%s: body %q is not a JSON object: %v", name, got.body, err)
	}
	if body.Error != "not found" {
		t.Errorf("%s: error field %q, want %q — the hint EXTENDS the DF-CRIER-213 envelope, it does not replace the message", name, body.Error, "not found")
	}
	if body.Hint == "" || body.DidYouMean == "" {
		t.Errorf("%s: body %q is missing the hint/did_you_mean pair the near miss owes", name, got.body)
	}
	return body, got
}

// TestWrongShapedPathNamesTheRightShape is the row's acceptance criterion, shape
// by shape: every request here is exactly one path segment away from a route the
// router serves, and every one must come back naming it.
func TestWrongShapedPathNamesTheRightShape(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	cases := []struct {
		name         string
		method       string
		target       string
		wantTemplate string
		wantHint     []string
	}{
		{
			// The reviewer's own first attempt, verbatim in shape: the topic
			// as a query parameter on a route that takes it as a path segment.
			name:         "query parameter where the path segment belongs (the review's first attempt)",
			method:       http.MethodGet,
			target:       "/relay/subscribe?topic=news",
			wantTemplate: "/relay/subscribe/{topic}",
			wantHint: []string{
				"GET /relay/subscribe is not a route",
				"/relay/subscribe/{topic}",
				"the topic belongs in the PATH, not in the query string",
				"?topic=news",
				"Try GET /relay/subscribe/news",
			},
		},
		{
			name:         "the same route with the value simply missing",
			method:       http.MethodGet,
			target:       "/relay/subscribe",
			wantTemplate: "/relay/subscribe/{topic}",
			wantHint: []string{
				"/relay/subscribe/{topic} is one path segment longer",
				"Append the topic as the last path segment",
			},
		},
		{
			name:         "path segment where a query parameter belongs",
			method:       http.MethodGet,
			target:       "/agents/crfeat031-probe/inbox/wait_seconds=30",
			wantTemplate: "/agents/{id}/inbox",
			wantHint: []string{
				"takes no trailing path segment",
				"wait_seconds is a QUERY parameter, not a path segment",
				"Try GET /agents/crfeat031-probe/inbox?wait_seconds=30",
			},
		},
		{
			name:         "a stray path segment on a route that takes none",
			method:       http.MethodGet,
			target:       "/agents/crfeat031-probe/inbox/30",
			wantTemplate: "/agents/{id}/inbox",
			wantHint: []string{
				"takes no trailing path segment",
				`"30" is not part of it`,
				"Try GET /agents/crfeat031-probe/inbox",
				// The documented options of that operation, read out of the
				// EMBEDDED OpenAPI spec — not a table in this file.
				"?limit, ?lease_seconds, ?wait_seconds",
			},
		},
		{
			name:         "a path segment where a JSON body field belongs",
			method:       http.MethodPost,
			target:       "/relay/publish/news",
			wantTemplate: "/relay/publish",
			wantHint: []string{
				"POST /relay/publish/news is not a route",
				"the payload goes in the JSON body (topic, event)",
			},
		},
		{
			name:         "a verb the named route does not serve is called out",
			method:       http.MethodGet,
			target:       "/relay/publish/news",
			wantTemplate: "/relay/publish",
			wantHint: []string{
				"That path serves POST, not GET",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := probeHint(t, client, tc.method, baseURL+tc.target)
			if body.DidYouMean != tc.wantTemplate {
				t.Errorf("did_you_mean = %q, want %q", body.DidYouMean, tc.wantTemplate)
			}
			for _, want := range tc.wantHint {
				if !strings.Contains(body.Hint, want) {
					t.Errorf("hint is missing %q\nhint: %s", want, body.Hint)
				}
			}
		})
	}
}

// TestShapeHintNamesARouteTheServerServes is the anti-prose half: `did_you_mean`
// must be a SHAPE, not a plausible-looking string. Substituting a value for each
// placeholder of the template the hint named must reach a route the router
// really serves — a 404 there would mean the hint sent the caller to another
// dead end, which is the exact failure this row exists to remove.
func TestShapeHintNamesARouteTheServerServes(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	for _, target := range []string{
		"/relay/subscribe?topic=news",
		"/relay/subscribe",
		"/agents/crfeat031-probe/inbox/30",
		"/agents/crfeat031-probe/inbox/wait_seconds=30",
		"/health/foo",
	} {
		body, _ := probeHint(t, client, http.MethodGet, baseURL+target)

		shaped := materializeTemplate(body.DidYouMean)
		if shaped == "" {
			t.Errorf("GET %s: did_you_mean %q has no placeholder to substitute", target, body.DidYouMean)
			continue
		}
		got := probeWire(t, client, http.MethodGet, baseURL+shaped, nil)
		if got.status == http.StatusNotFound {
			t.Errorf("GET %s: did_you_mean %q materializes to %s, which is ITSELF a 404 — the hint names a shape the server does not serve",
				target, body.DidYouMean, shaped)
		}
	}
}

// materializeTemplate substitutes every `{placeholder}` segment of a route
// template with a probe value, so the caller can ask the router whether that
// shape exists. Segments without a placeholder are left alone.
func materializeTemplate(template string) string {
	segs := pathSegments(template)
	if len(segs) == 0 {
		return ""
	}
	for i, seg := range segs {
		if isPlaceholder(seg) {
			segs[i] = "crfeat031-probe"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// TestPathsThatAreNotNearMissesKeepTheExactBody is the negative control for the
// hint: the overwhelmingly common 404 — a path that looks like nothing the
// router serves — must keep the byte-identical DF-CRIER-213 envelope. Without
// this, a hint printed unconditionally would satisfy the tests above and prove
// nothing about the near-miss classification.
//
// Where the boundary sits is pinned by BOTH halves: every path below differs
// from every registered template in a LITERAL segment (or in length by more than
// one), so no shape can be named; and
// TestExtraSegmentOnAPrefixSharingPathStillHints states the case that does hint,
// so the boundary is deliberate and visible rather than an accident of a
// less specific assertion.
func TestPathsThatAreNotNearMissesKeepTheExactBody(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	for _, target := range []string{
		"/nope",           // no route starts with "nope"
		"/nope/deeper",    // …at any depth
		"/relay/subscrib", // a typo in a literal segment is not a shape question
		"/healthx",        // neither is a longer literal
		"/drl/health",     // a wrong first segment can never be "one segment off"
	} {
		got := probeWire(t, client, http.MethodGet, baseURL+target, nil)
		assertJSONErrorEnvelope(t, "GET "+target, got, http.StatusNotFound, "not found")
		if strings.Contains(got.body, "did_you_mean") || strings.Contains(got.body, "hint") {
			t.Errorf("GET %s: body %q carries hint fields for a path that is not one segment away from any route", target, got.body)
		}
	}
}

// TestExtraSegmentOnAPrefixSharingPathStillHints: the boundary the negative
// control above stops at. `/agents/one/two` differs from `/agents/{id}` by one
// trailing segment, so it IS a near miss and IS hinted — the answer is true
// (that route takes no trailing segment) and it names the route the guess was
// aimed at. It is asserted here so that behaviour is a decision with a test, not
// an emergent side of the matcher.
func TestExtraSegmentOnAPrefixSharingPathStillHints(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	body, _ := probeHint(t, client, http.MethodGet, baseURL+"/agents/one/two")
	if body.DidYouMean != "/agents/{id}" {
		t.Errorf("did_you_mean = %q, want %q", body.DidYouMean, "/agents/{id}")
	}
	if !strings.Contains(body.Hint, "takes no trailing path segment") {
		t.Errorf("hint does not say the route ends before the stray segment: %s", body.Hint)
	}
}

// TestWrongMethodAnswerKeepsItsShape: the hint machinery is 404-only. A 405 is
// the *other* shape answer and it already names the shape in its Allow header,
// so its body must stay exactly what DF-CRIER-213 shipped — no hint fields, no
// second opinion next to Allow.
func TestWrongMethodAnswerKeepsItsShape(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	got := probeWire(t, client, http.MethodDelete, baseURL+"/health", nil)
	assertJSONErrorEnvelope(t, "DELETE /health", got, http.StatusMethodNotAllowed, "method not allowed")
	if allow := got.header.Get("Allow"); allow != "GET" {
		t.Errorf("DELETE /health: Allow %q, want %q (derived from the router, not fabricated)", allow, "GET")
	}
	if strings.Contains(got.body, "did_you_mean") {
		t.Errorf("DELETE /health: 405 body %q carries a route hint — the Allow header is that answer", got.body)
	}
}

// TestMatchedRoutesAreUnaffectedByTheHint: the correct shapes must still be
// served by their handlers. The router's own routes — including the one the
// review was reaching for — answer, not this fallback.
func TestMatchedRoutesAreUnaffectedByTheHint(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	// /relay/subscribe/{topic} IS a route: a plain GET without the WebSocket
	// handshake is answered by the upgrader (a 400), never by the fallback.
	if got := probeWire(t, client, http.MethodGet, baseURL+"/relay/subscribe/crfeat031-topic", nil); got.status == http.StatusNotFound {
		t.Errorf("GET /relay/subscribe/crfeat031-topic: 404 — the shape the hint names is not served")
	}
	// The documented retrieve route with the id in the path.
	if got := probeWire(t, client, http.MethodGet, baseURL+"/agents/crfeat031-probe/inbox", nil); got.status != http.StatusUnauthorized {
		t.Errorf("GET /agents/crfeat031-probe/inbox: status %d, want 401 (missing agent signature — a matched route, not the fallback)", got.status)
	}
	health := probeWire(t, client, http.MethodGet, baseURL+"/health", nil)
	if health.status != http.StatusOK || health.body != `{"status":"ok"}` {
		t.Errorf("GET /health: status %d body %q, want 200 {\"status\":\"ok\"}", health.status, health.body)
	}
}

// TestShapeHintEchoesNothingUnboundedOrUnsafe: a hint is a RESPONSE BODY built
// partly from the request line, which the caller controls. The echoed value must
// be bounded, must not carry control characters into the body, and must be
// escaped for the context it is moved into — a raw "/" pasted into the suggested
// path would name a different (still 404) shape.
func TestShapeHintEchoesNothingUnboundedOrUnsafe(t *testing.T) {
	baseURL := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	// An unbounded query value.
	huge := strings.Repeat("A", 4096)
	body, raw := probeHint(t, client, http.MethodGet, baseURL+"/relay/subscribe?topic="+huge)
	if len(raw.body) > 512 {
		t.Errorf("a 4096-byte query value produced a %d-byte 404 body; the echo must be bounded\nbody: %s", len(raw.body), raw.body)
	}
	if strings.Contains(body.Hint, huge) {
		t.Error("the hint reflects the whole unbounded query value")
	}

	// %0A decodes to a real newline. Only the envelope's own terminating
	// newline may appear in the body.
	_, raw = probeHint(t, client, http.MethodGet, baseURL+"/relay/subscribe?topic=ev%0Ail")
	inner := strings.TrimSuffix(raw.body, "\n")
	if strings.ContainsAny(inner, "\n\r	") {
		t.Errorf("a percent-encoded newline reached the 404 body as a control character: %q", raw.body)
	}
	if !strings.Contains(inner, "ev?il") {
		t.Errorf("the sanitized echo is missing from the body: %q", raw.body)
	}

	// An encoded "/" must be re-escaped for the PATH it is suggested in: the raw
	// value would name a different shape.
	_, raw = probeHint(t, client, http.MethodGet, baseURL+"/relay/subscribe?topic=a%2Fb")
	if !strings.Contains(raw.body, "/relay/subscribe/a%2Fb") {
		t.Errorf("the suggested path does not re-escape the value it moved into the path: %q", raw.body)
	}
	if strings.Contains(raw.body, "/relay/subscribe/a/b") {
		t.Errorf("the suggested path pastes a raw \"/\" into the path, naming a different shape: %q", raw.body)
	}
}
