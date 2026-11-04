package harness

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds `--version`. A probe that hangs must not hang a CLI
// command or a capability table.
const probeTimeout = 5 * time.Second

// probeBinary measures one harness binary without starting a session: the path
// resolves, and `--version` runs. Capability.Available is false with a Reason
// that names the exact binary when it cannot.
func probeBinary(ctx context.Context, binary string, features map[string]bool) (Capability, error) {
	cap := Capability{Binary: binary, Features: features}
	path, err := lookupBinary(binary)
	if err != nil {
		cap.Reason = "binary not found: " + binary
		if binary == "" {
			cap.Reason = "no binary configured"
		}
		return cap, nil
	}
	cap.Binary = path
	res := runCommand(ctx, execSpec{Path: path, Args: []string{"--version"}}, probeTimeout)
	if res.StartError != "" {
		cap.Reason = "cannot run " + path + ": " + res.StartError
		return cap, nil
	}
	if res.ExitCode != 0 {
		cap.Reason = "version probe exited " + strconv.Itoa(res.ExitCode)
		if res.Tail != "" {
			cap.Reason += ": " + res.Tail
		}
		return cap, nil
	}
	cap.Available = true
	cap.Version = versionFrom(string(res.Stdout))
	return cap, nil
}

// versionFrom reads a version out of a `--version` line: the last token, which
// is the version in every shape these CLIs print ("opencode 1.18.29",
// "hermes 0.9.1", "1.2.3").
func versionFrom(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		return strings.TrimPrefix(fields[len(fields)-1], "v")
	}
	return ""
}

// sessionUnknownMarkers are the harness-side classes that mean "no such
// session" (§4.3). When one appears the adapter reports
// HARNESS_SESSION_UNKNOWN and never creates a replacement.
var sessionUnknownMarkers = []string{
	"session not found",
	"unknown session",
	"no such session",
	"session does not exist",
	"no session found",
}

// ClassifyFailure maps a failed run onto the §5 vocabulary: an unknown session
// gets its own code so a caller can tell "this session is gone" from "the
// harness errored".
func ClassifyFailure(stdout, stderr string) string {
	hay := strings.ToLower(stdout + "\n" + stderr)
	for _, marker := range sessionUnknownMarkers {
		if strings.Contains(hay, marker) {
			return CodeSessionUnknown
		}
	}
	return CodeExitNonZero
}

// ExtractSessionID pulls a harness-side session id out of `--format json`
// output. It accepts a JSON document, one JSON object, or JSON lines, and
// looks only at the documented session keys so an arbitrary `id` on an event
// (a message id, a tool id) is not mistaken for a session.
func ExtractSessionID(stdout []byte) string {
	text := strings.TrimSpace(string(stdout))
	if text == "" {
		return ""
	}
	if id := sessionIDFromJSON([]byte(text)); id != "" {
		return id
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		if id := sessionIDFromJSON([]byte(line)); id != "" {
			return id
		}
	}
	return ""
}

func sessionIDFromJSON(raw []byte) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	// A wrapper object ({"session": {...}}, {"data": {...}}) is searched one
	// level deep before giving up.
	for _, key := range []string{"sessionID", "session_id", "sessionId"} {
		if id := stringField(obj, key); id != "" {
			return id
		}
	}
	for _, wrapper := range []string{"session", "data", "result"} {
		if nested, ok := obj[wrapper]; ok {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(nested, &inner); err == nil {
				for _, key := range []string{"sessionID", "session_id", "sessionId", "id"} {
					if id := stringField(inner, key); id != "" {
						return id
					}
				}
			}
		}
	}
	// Last resort: a response envelope whose identifier is simply `id` (the
	// Hermes gateway's /v1/responses object). Only reached when no documented
	// session key exists anywhere.
	if id := stringField(obj, "id"); id != "" {
		return id
	}
	return ""
}

func stringField(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}
