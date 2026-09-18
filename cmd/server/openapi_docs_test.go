package main

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAPIDocsIndexesEveryPath is the DF-CRIER-196 gate. Before this change
// /docs was a stub: it linked the spec documents but indexed no endpoint at
// all, so a reader learned nothing about the API surface from it.
//
// The expectation is parsed from the SAME embedded spec the server serves
// (openapiYAML → openapiJSON), never from a hand-copied list of paths — that is
// what makes a spec path which never reaches the page fail here instead of
// drifting silently. The page is rendered once at startup, so the test drives
// the real handler rather than a copy of the constant.
func TestOpenAPIDocsIndexesEveryPath(t *testing.T) {
	rec := httptest.NewRecorder()
	handleOpenAPIDocs(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs: status %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET /docs: Content-Type %q, want %q", ct, "text/html; charset=utf-8")
	}
	body := rec.Body.String()

	// The page still links both machine-readable formats (the pre-existing
	// contract this change must not break).
	for _, href := range []string{`href="/openapi.json"`, `href="/openapi.yaml"`} {
		if !strings.Contains(body, href) {
			t.Errorf("/docs no longer links %s", href)
		}
	}

	// …and still states that the auth-exempt paths are public.
	if !strings.Contains(body, "no auth token required") {
		t.Error("/docs no longer states that the auth-exempt paths are public")
	}

	// HARD constraint: the page must stay self-contained and work offline over
	// plain HTTP, so it may reference no external or scripted asset at all.
	// (A spec summary that merely mentions a URL is not an asset reference —
	// this checks the reference shapes, not the substring "http".)
	for _, bad := range []string{"<script", "<link", "src=", "@import", "url(http", `href="http`, "href='http"} {
		if strings.Contains(body, bad) {
			t.Errorf("/docs references an external or scripted asset (%q) — the page must stay self-contained", bad)
		}
	}

	assertDocsIndexesSpec(t, body)
}

// assertDocsIndexesSpec checks that the served /docs body lists every path and
// operation of the embedded spec, with the operation summary the spec gives it.
// It is shared by the handler-level test above and the live-server subtest in
// TestOpenAPIServed so both are pinned to one expectation source.
func assertDocsIndexesSpec(t *testing.T, body string) {
	t.Helper()

	var spec struct {
		Paths map[string]map[string]struct {
			Summary string `json:"summary"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openapiJSON, &spec); err != nil {
		t.Fatalf("parse the embedded spec as JSON: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("the embedded spec declares no paths — this gate would be vacuous")
	}

	rows := docsIndexRows(body)
	paths, ops := 0, 0
	for path, item := range spec.Paths {
		paths++
		for method, op := range item {
			upper := strings.ToUpper(method)
			if !httpMethods[upper] {
				continue
			}
			ops++
			summary := strings.Join(strings.Fields(op.Summary), " ")
			found := false
			for _, row := range rows {
				// The rendered row is "METHOD path summary…"; match on the
				// method and path CELLS exactly (a prefix match would let
				// "POST /agents" be satisfied by a "POST /agents/{id}/inbox"
				// row).
				fields := strings.Fields(row)
				if len(fields) < 2 || fields[0] != upper || fields[1] != path {
					continue
				}
				found = true
				if summary != "" && !strings.Contains(row, summary) {
					t.Errorf("/docs row %q does not carry the spec summary %q", row, summary)
				}
			}
			if !found {
				t.Errorf("/docs does not index %s %s (spec summary %q)", upper, path, op.Summary)
			}
		}
	}
	if ops == 0 {
		t.Fatal("the embedded spec declares no operations — this gate would be vacuous")
	}
	t.Logf("/docs indexes %d paths and %d operations from the embedded spec", paths, ops)
}

// docsIndexRows flattens the /docs index's table rows to normalized text, e.g.
// "GET /health Health check", so the assertions do not depend on the page's
// markup beyond its rows.
func docsIndexRows(body string) []string {
	var rows []string
	for _, chunk := range strings.Split(body, "<tr>")[1:] {
		row, _, _ := strings.Cut(chunk, "</tr>")
		row = html.UnescapeString(stripTags(row))
		rows = append(rows, strings.Join(strings.Fields(row), " "))
	}
	return rows
}

// stripTags removes HTML tags, leaving their text content behind.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
			b.WriteRune(' ')
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}
