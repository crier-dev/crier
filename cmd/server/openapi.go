package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

// The OpenAPI spec lives at docs/openapi.yaml (the source of truth). go:embed
// cannot reach outside the package directory, so the Makefile `generate`
// target (go generate ./...) copies it here; the CI openapi-spec-validator
// job re-runs the copy and fails on any drift.
//
//go:generate cp ../../docs/openapi.yaml openapi.yaml

//go:embed openapi.yaml
var openapiYAML []byte

// openapiJSON is the spec decoded from YAML and re-marshaled as JSON. It is
// computed once at startup so every /openapi.json response shares one
// allocation and the marshal can never fail mid-request.
var openapiJSON []byte

// apiOperation is one operation of the embedded spec, in the spec's own
// declaration order.
type apiOperation struct {
	Method  string
	Path    string
	Summary string
}

// docsPage is the /docs template's data model: every operation of the embedded
// spec in spec order, plus the counts the page prints.
type docsPage struct {
	Operations []apiOperation
	PathCount  int
	OpCount    int
}

// openapiDocsHTML is the fully rendered /docs page, built once at startup from
// the embedded spec. Every request writes these bytes unchanged, so the
// handler allocates nothing per request.
var openapiDocsHTML []byte

// httpMethods are the OpenAPI path-item keys that denote an operation.
// Anything else on a path item (parameters, summary, servers, $ref) is
// metadata, not an operation, and is skipped.
var httpMethods = map[string]bool{
	"GET": true, "PUT": true, "POST": true, "DELETE": true,
	"OPTIONS": true, "HEAD": true, "PATCH": true, "TRACE": true,
}

func init() {
	var doc any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		// The embedded spec is generated from docs/openapi.yaml; a parse
		// failure here is a wiring error caught at startup, not a request
		// error.
		panic("openapi: invalid embedded YAML: " + err.Error())
	}
	var err error
	openapiJSON, err = json.Marshal(doc)
	if err != nil {
		panic("openapi: cannot marshal spec to JSON: " + err.Error())
	}
	openapiDocsHTML = renderDocsPage()
}

// handleOpenAPIJSON serves the spec as JSON (Content-Type application/json).
func handleOpenAPIJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}

// handleOpenAPIYAML serves the raw embedded spec bytes.
func handleOpenAPIYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(openapiYAML)
}

// handleOpenAPIDocs serves the self-contained HTML index of the API. The page
// is rendered at startup from the embedded spec (see renderDocsPage), so it
// lists every path and operation the spec declares and links both
// machine-readable endpoints. No CDN / Swagger-UI dependency and no external
// asset reference of any kind — it works fully offline. It is a static index,
// not an interactive request console.
func handleOpenAPIDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(openapiDocsHTML)
}

// specOperations walks the embedded spec's `paths` mapping in document order
// and returns one apiOperation per (path, method) pair, plus the number of
// paths declared. It panics on a spec it cannot index: /docs must never be
// served as a silently empty index.
func specOperations(spec []byte) ([]apiOperation, int) {
	var root yaml.Node
	if err := yaml.Unmarshal(spec, &root); err != nil {
		panic("openapi: invalid embedded YAML: " + err.Error())
	}
	if len(root.Content) == 0 {
		panic("openapi: embedded spec is empty")
	}
	paths := mappingValue(root.Content[0], "paths")
	if paths == nil || paths.Kind != yaml.MappingNode {
		panic("openapi: embedded spec declares no `paths` mapping")
	}
	ops := make([]apiOperation, 0, len(paths.Content)/2)
	for i := 0; i+1 < len(paths.Content); i += 2 {
		path := paths.Content[i].Value
		item := paths.Content[i+1]
		if item.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			method := strings.ToUpper(item.Content[j].Value)
			if !httpMethods[method] {
				continue
			}
			summary := ""
			if op := item.Content[j+1]; op.Kind == yaml.MappingNode {
				if s := mappingValue(op, "summary"); s != nil {
					summary = s.Value
				}
			}
			ops = append(ops, apiOperation{Method: method, Path: path, Summary: summary})
		}
	}
	if len(ops) == 0 {
		panic("openapi: embedded spec declares no operations — /docs would index nothing")
	}
	return ops, len(paths.Content) / 2
}

// mappingValue returns the value node of key in a YAML mapping node, or nil
// when node is not a mapping or the key is absent.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// renderDocsPage renders the static /docs page from the embedded spec. Every
// value interpolated into the template is spec-authored text; html/template
// escapes it, so a summary cannot inject markup into the page.
func renderDocsPage() []byte {
	ops, pathCount := specOperations(openapiYAML)
	var buf bytes.Buffer
	if err := openapiDocsTemplate.Execute(&buf, docsPage{
		Operations: ops,
		PathCount:  pathCount,
		OpCount:    len(ops),
	}); err != nil {
		panic("openapi: cannot render /docs: " + err.Error())
	}
	return buf.Bytes()
}

// openapiDocsTemplate is the /docs page. It is deliberately asset-free: no
// script, no external stylesheet, no font URL — the CSS is inline and the
// index is rendered server-side into the HTML, so the page works offline and
// over plain HTTP.
var openapiDocsTemplate = template.Must(template.New("docs").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Crier — API reference</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 60rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; color: #1a1a1a; }
  code, a { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  a { color: #1a56db; }
  table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
  th, td { text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid #e5e7eb; vertical-align: top; }
  th { border-bottom: 2px solid #d1d5db; }
  td.method { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; white-space: nowrap; }
</style>
</head>
<body>
<h1>Crier — API reference</h1>
<p>The Crier API is described by an OpenAPI 3.1 document. This server serves the spec live in two machine-readable formats:</p>
<ul>
  <li><a href="/openapi.json">/openapi.json</a> — the spec as JSON</li>
  <li><a href="/openapi.yaml">/openapi.yaml</a> — the spec as YAML</li>
</ul>
<h2>Endpoints</h2>
<p>All {{.PathCount}} paths and {{.OpCount}} operations the specification declares, in the spec's own order, each with its summary from the spec:</p>
<table>
<thead>
<tr><th>Method</th><th>Path</th><th>Summary</th></tr>
</thead>
<tbody>
{{range .Operations}}<tr><td class="method">{{.Method}}</td><td><code>{{.Path}}</code></td><td>{{.Summary}}</td></tr>
{{end}}</tbody>
</table>
<p>This index is generated from that spec at startup. It is a static reference page, <em>not</em> an interactive request console — fire your requests with curl or any HTTP client. This page and the other auth-exempt endpoints (<code>/health</code>, <code>/version</code>, <code>/openapi.json</code>, <code>/openapi.yaml</code>) are public — no auth token required. This page is self-contained and works offline (no CDN / Swagger-UI dependency).</p>
</body>
</html>
`))
