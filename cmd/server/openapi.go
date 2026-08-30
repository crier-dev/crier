package main

import (
	_ "embed"
	"encoding/json"
	"net/http"

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

// handleOpenAPIDocs serves a minimal self-contained HTML landing page linking
// to both machine-readable endpoints. No CDN / Swagger-UI dependency — it
// works fully offline.
func handleOpenAPIDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(openAPIDocsHTML))
}

const openAPIDocsHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Crier — OpenAPI specification</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 60rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; color: #1a1a1a; }
  code, a { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  a { color: #1a56db; }
</style>
</head>
<body>
<h1>Crier — OpenAPI specification</h1>
<p>The Crier API is described by an OpenAPI 3.1 document. This server serves the spec live in two machine-readable formats:</p>
<ul>
  <li><a href="/openapi.json">/openapi.json</a> — the spec as JSON</li>
  <li><a href="/openapi.yaml">/openapi.yaml</a> — the spec as YAML</li>
</ul>
<p>All three endpoints (<code>/openapi.json</code>, <code>/openapi.yaml</code>, <code>/docs</code>) are public — no auth token required. This page is self-contained and works offline (no CDN / Swagger-UI dependency).</p>
</body>
</html>
`
