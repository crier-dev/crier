package main

import (
	"encoding/json"
	"net/http"

	"github.com/crier-dev/crier/internal/buildinfo"
)

// handleVersion serves the running build's identity as JSON (DF-CRIER-101):
//
//	{"version":"1.2.3","commit":"1a2b3c4d","build_time":"2026-09-14T06:05:59Z","modified":false}
//
// The values come from internal/buildinfo, the same source the -version flag
// prints, so the CLI and the HTTP surface can never disagree about which build
// is running. Like /health, the route is exempt from middleware.Auth: an
// operator asking a live server what it is must not need a token first.
func handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Encoding a fixed-shape struct of a string/string/string/bool cannot
	// fail; a write error here means the client went away, which the
	// connection layer already reports.
	_ = json.NewEncoder(w).Encode(buildinfo.Resolve())
}
