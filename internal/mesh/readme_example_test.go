package mesh

// readme_example_test.go — DF-CRIER-3: the README's mesh section and the shipped
// wire types are one contract, so this gate executes them against each other.
//
// The README quotes the frames a client has to send and accept. A doc edit that
// renames a field, drops the correlation rule, or invents a shape that
// internal/mesh/message.go does not define is now a build failure instead of a
// tester's dead end. The frames live between two markers in README.md:
//
//	<!-- mesh-frames:start -->
//	```json
//	{...}
//	```
//	<!-- mesh-frames:end -->
//
// Extraction is deliberately literal: everything between the markers that starts
// with "{" is a frame (the ```json fence line and any prose the doc grows around
// them are skipped), and a region that yields ZERO frames FAILS — a gate that
// reads nothing must never pass. The gate is not a markdown parser; it is a
// decoder pinning field names to the structs.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	readmeFramesStart = "<!-- mesh-frames:start -->"
	readmeFramesEnd   = "<!-- mesh-frames:end -->"
)

// readme returns the repo README (this package sits two levels below the root).
func readme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(raw)
}

// extractMeshFrames pulls the frames out of a doc's marked mesh region, keyed by
// their `type`. It is pure so the refusal paths below can be proven without
// editing README.md.
func extractMeshFrames(doc string) (map[string]json.RawMessage, error) {
	start := strings.Index(doc, readmeFramesStart)
	if start < 0 {
		return nil, fmt.Errorf("no %s marker — the mesh example must stay delimited for this gate", readmeFramesStart)
	}
	rest := doc[start+len(readmeFramesStart):]
	end := strings.Index(rest, readmeFramesEnd)
	if end < 0 {
		return nil, fmt.Errorf("the mesh example opens with %s but never closes it with %s", readmeFramesStart, readmeFramesEnd)
	}

	frames := map[string]json.RawMessage{}
	for _, line := range strings.Split(rest[:end], "\n") {
		// Only JSON object lines are frames: the region also carries the ```json
		// fence and whatever prose the surrounding section grows.
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if !json.Valid([]byte(line)) {
			return nil, fmt.Errorf("a line in the mesh example is not valid JSON: %q", line)
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			return nil, fmt.Errorf("frame has no readable type: %v (%q)", err, line)
		}
		if probe.Type == "" {
			return nil, fmt.Errorf("frame carries no `type`: %q", line)
		}
		if _, dup := frames[probe.Type]; dup {
			return nil, fmt.Errorf("more than one %s frame is quoted — this gate keys frames by type", probe.Type)
		}
		frames[probe.Type] = json.RawMessage(line)
	}

	if len(frames) == 0 {
		return nil, fmt.Errorf("the marked region between %s and %s contains no JSON frame — this gate would have passed over an empty example", readmeFramesStart, readmeFramesEnd)
	}
	return frames, nil
}

// readmeMeshFrames extracts the marked example's frames from the repo README.
func readmeMeshFrames(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	frames, err := extractMeshFrames(readme(t))
	if err != nil {
		t.Fatalf("README.md mesh example: %v", err)
	}
	return frames
}

// decodeStrict decodes a quoted frame into the real wire type, rejecting a field
// name that the struct does not define.
func decodeStrict(t *testing.T, frame json.RawMessage, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(frame))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("README frame does not decode into %T: %v\n  frame: %s\n  (every quoted field name must exist in internal/mesh/message.go)", v, err, frame)
	}
}

// isMessageID reports whether s looks like the 24-hex id message.go generates.
func isMessageID(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// isLowerHex reports whether s is non-empty and made only of lowercase hex
// digits — the shape this package puts on the wire for a nonce and for an
// ed25519 signature (hex.EncodeToString).
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// TestReadmeMeshFramesDecodeIntoTheWireTypes is the core gate: every frame the
// README quotes must be a frame this package can marshal and unmarshal, and must
// carry the fields the README's field table promises.
func TestReadmeMeshFramesDecodeIntoTheWireTypes(t *testing.T) {
	frames := readmeMeshFrames(t)

	// --- REGISTER ---------------------------------------------------------
	regRaw, ok := frames["REGISTER"]
	if !ok {
		t.Errorf("the README mesh example must quote a REGISTER frame (it is the handshake a client sends first)")
	} else {
		var reg Register
		decodeStrict(t, regRaw, &reg)
		assertFrame(t, "REGISTER", func() []string {
			var bad []string
			if reg.Type != TypeRegister {
				bad = append(bad, fmt.Sprintf("type=%q", reg.Type))
			}
			if reg.Version != 1 {
				bad = append(bad, fmt.Sprintf("version=%d", reg.Version))
			}
			if !isMessageID(reg.MessageID) {
				bad = append(bad, fmt.Sprintf("message_id=%q is not a 24-hex id", reg.MessageID))
			}
			if reg.Timestamp.IsZero() {
				bad = append(bad, "timestamp is missing")
			}
			if reg.AgentID == "" {
				bad = append(bad, "agent_id is empty")
			}
			if reg.LeaseTTLMs == 0 {
				bad = append(bad, "lease_ttl_ms is 0")
			}
			if reg.Capabilities.Version == "" {
				bad = append(bad, "capabilities.version is empty")
			}
			if reg.Capabilities.MaxConcurrentSessions == 0 {
				bad = append(bad, "capabilities.max_concurrent_sessions is 0")
			}
			return bad
		})
	}

	// --- REQUEST ----------------------------------------------------------
	reqRaw, ok := frames["REQUEST"]
	if !ok {
		t.Fatalf("the README mesh example must quote a REQUEST frame — it is the frame the whole section exists to show")
	}
	var req Request
	decodeStrict(t, reqRaw, &req)
	assertFrame(t, "REQUEST", func() []string {
		var bad []string
		if req.Type != TypeRequest {
			bad = append(bad, fmt.Sprintf("type=%q", req.Type))
		}
		if req.Version != 1 {
			bad = append(bad, fmt.Sprintf("version=%d", req.Version))
		}
		if !isMessageID(req.MessageID) {
			bad = append(bad, fmt.Sprintf("message_id=%q is not a 24-hex id", req.MessageID))
		}
		if req.Timestamp.IsZero() {
			bad = append(bad, "timestamp is missing")
		}
		if req.Source.AgentID == "" {
			bad = append(bad, "source.agent_id is empty")
		}
		if req.Target.AgentID == "" {
			bad = append(bad, "target.agent_id is empty (the server routes on it)")
		}
		if req.Method == "" {
			bad = append(bad, "method is empty")
		}
		if req.Path == "" {
			bad = append(bad, "path is empty")
		}
		if req.Body == nil {
			bad = append(bad, "body is absent (the example documents an object body)")
		}
		if req.TraceID == "" {
			bad = append(bad, "trace_id is empty")
		}
		if req.TimeoutMs == 0 {
			bad = append(bad, "timeout_ms is 0")
		}
		return bad
	})

	// --- RESPONSE (and the correlation contract it carries) ---------------
	respRaw, ok := frames["RESPONSE"]
	if !ok {
		t.Fatalf("the README mesh example must quote a RESPONSE frame — the reply is what the correlation rule is about")
	}
	var resp Response
	decodeStrict(t, respRaw, &resp)
	assertFrame(t, "RESPONSE", func() []string {
		var bad []string
		if resp.Type != TypeResponse {
			bad = append(bad, fmt.Sprintf("type=%q", resp.Type))
		}
		if resp.Version != 1 {
			bad = append(bad, fmt.Sprintf("version=%d", resp.Version))
		}
		if !isMessageID(resp.MessageID) {
			bad = append(bad, fmt.Sprintf("message_id=%q is not a 24-hex id", resp.MessageID))
		}
		if resp.Timestamp.IsZero() {
			bad = append(bad, "timestamp is missing")
		}
		if !isMessageID(resp.RequestID) {
			bad = append(bad, fmt.Sprintf("request_id=%q is not a 24-hex id", resp.RequestID))
		}
		if resp.Source.AgentID == "" {
			bad = append(bad, "source.agent_id is empty")
		}
		if resp.StatusCode == 0 {
			bad = append(bad, "status_code is 0 (a RESPONSE must carry the responder's code)")
		}
		if len(resp.Body) == 0 {
			bad = append(bad, "body is absent (the example documents an object body)")
		}
		if resp.TraceID == "" {
			bad = append(bad, "trace_id is empty")
		}
		return bad
	})

	// (c) the documented correlation, on the quoted frames themselves.
	if resp.RequestID != req.MessageID {
		t.Errorf("the README's correlation rule is not true of its own example: RESPONSE.request_id=%q, REQUEST.message_id=%q — the two must be the same string",
			resp.RequestID, req.MessageID)
	}
	if resp.MessageID == resp.RequestID {
		t.Errorf("the README's RESPONSE uses the same id for its own message_id and its request_id (%q) — the example could not show which field correlates", resp.MessageID)
	}

	// --- KEEPALIVE --------------------------------------------------------
	kaRaw, ok := frames["KEEPALIVE"]
	if !ok {
		t.Errorf("the README mesh example must quote a KEEPALIVE frame — it is the frame a client has to ignore while it waits")
	} else {
		var ka Keepalive
		decodeStrict(t, kaRaw, &ka)
		assertFrame(t, "KEEPALIVE", func() []string {
			var bad []string
			if ka.Type != TypeKeepalive {
				bad = append(bad, fmt.Sprintf("type=%q", ka.Type))
			}
			if ka.Version != 1 {
				bad = append(bad, fmt.Sprintf("version=%d", ka.Version))
			}
			if !isMessageID(ka.MessageID) {
				bad = append(bad, fmt.Sprintf("message_id=%q is not a 24-hex id", ka.MessageID))
			}
			if ka.Timestamp.IsZero() {
				bad = append(bad, "timestamp is missing")
			}
			if ka.AgentID == "" {
				bad = append(bad, "agent_id is empty (the sender's identity)")
			}
			return bad
		})
		// The whole reason a KEEPALIVE can be filtered by type is that it has no
		// correlation field: if one ever gained `request_id`, a client matching
		// on it could mistake the keepalive for its reply.
		var asReply struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(kaRaw, &asReply); err == nil && asReply.RequestID != "" {
			t.Errorf("the README's KEEPALIVE frame carries a request_id (%q) — that field is what makes a frame a reply", asReply.RequestID)
		}
	}

	// --- ERROR (quoted, so it is gated too) -------------------------------
	if errRaw, ok := frames["ERROR"]; ok {
		var errMsg ErrorMessage
		decodeStrict(t, errRaw, &errMsg)
		assertFrame(t, "ERROR", func() []string {
			var bad []string
			if errMsg.Type != TypeError {
				bad = append(bad, fmt.Sprintf("type=%q", errMsg.Type))
			}
			if !isMessageID(errMsg.RequestID) {
				bad = append(bad, fmt.Sprintf("request_id=%q is not a 24-hex id", errMsg.RequestID))
			}
			if errMsg.Error.Code == "" {
				bad = append(bad, "error.code is empty")
			}
			if errMsg.Error.Message == "" {
				bad = append(bad, "error.message is empty")
			}
			if errMsg.MessageID == errMsg.RequestID {
				bad = append(bad, "message_id and request_id are the same value")
			}
			return bad
		})
		// ERROR frames correlate exactly like RESPONSE frames (one forwarding
		// path serves both), so the README's example must show that.
		if errMsg.RequestID != req.MessageID {
			t.Errorf("the README must show an ERROR correlating like a RESPONSE: ERROR.request_id=%q, REQUEST.message_id=%q — the doc claims they are identical",
				errMsg.RequestID, req.MessageID)
		}
		if errMsg.Error.Code != ErrCodeControllerOffline {
			t.Errorf("the quoted ERROR's code %q is not one this package defines (e.g. %s)", errMsg.Error.Code, ErrCodeControllerOffline)
		}
	}

	// --- the AUTH_* handshake frames (DF-CRIER-287) -----------------------
	// These three appear only on a mesh with CR_REQUIRE_MESH_AUTH=true, but the
	// README quotes them because a client that connects to such a server meets
	// them FIRST: if the doc's shapes drift from the wire types, a client's
	// handshake dies before any of the frames above ever arrives.
	if raw, ok := frames["AUTH_CHALLENGE"]; !ok {
		t.Errorf("the README mesh example must quote an AUTH_CHALLENGE frame — it is the first frame a client sees on an authenticated mesh")
	} else {
		var ch AuthChallenge
		decodeStrict(t, raw, &ch)
		assertFrame(t, "AUTH_CHALLENGE", func() []string {
			var bad []string
			if ch.Type != TypeAuthChallenge {
				bad = append(bad, fmt.Sprintf("type=%q", ch.Type))
			}
			if ch.Version != 1 {
				bad = append(bad, fmt.Sprintf("version=%d", ch.Version))
			}
			if !isMessageID(ch.MessageID) {
				bad = append(bad, fmt.Sprintf("message_id=%q is not a 24-hex id", ch.MessageID))
			}
			if ch.AgentID == "" {
				bad = append(bad, "agent_id is empty (the path identity under test)")
			}
			if len(ch.Nonce) != 32 || !isLowerHex(ch.Nonce) {
				bad = append(bad, fmt.Sprintf("nonce=%q is not 32 hex chars", ch.Nonce))
			}
			if ch.ExpiresAt.IsZero() {
				bad = append(bad, "expires_at is missing")
			}
			return bad
		})
	}

	if raw, ok := frames["AUTH_RESPONSE"]; !ok {
		t.Errorf("the README mesh example must quote an AUTH_RESPONSE frame — it is the frame a client must send to be admitted")
	} else {
		var ar AuthResponse
		decodeStrict(t, raw, &ar)
		assertFrame(t, "AUTH_RESPONSE", func() []string {
			var bad []string
			if ar.Type != TypeAuthResponse {
				bad = append(bad, fmt.Sprintf("type=%q", ar.Type))
			}
			if ar.AgentID == "" {
				bad = append(bad, "agent_id is empty")
			}
			if len(ar.Nonce) != 32 || !isLowerHex(ar.Nonce) {
				bad = append(bad, fmt.Sprintf("nonce=%q is not 32 hex chars", ar.Nonce))
			}
			// The signature is the hex encoding of a 64-byte ed25519 signature:
			// 128 hex chars. The README's own example has to be signable-shaped,
			// or a reader copies a placeholder that can never verify.
			if len(ar.Signature) != 128 || !isLowerHex(ar.Signature) {
				bad = append(bad, fmt.Sprintf("signature is %d characters, want 128 hex chars (64-byte ed25519)", len(ar.Signature)))
			}
			return bad
		})
		// The doc teaches "echo the nonce you were challenged with": the two
		// quoted frames must agree, or the example cannot complete.
		if chRaw, ok := frames["AUTH_CHALLENGE"]; ok {
			var ch AuthChallenge
			decodeStrict(t, chRaw, &ch)
			if ar.Nonce != ch.Nonce {
				t.Errorf("the README's AUTH_RESPONSE nonce (%q) does not echo its AUTH_CHALLENGE nonce (%q) — the example would be refused", ar.Nonce, ch.Nonce)
			}
			if ar.AgentID != ch.AgentID {
				t.Errorf("the README's AUTH_RESPONSE names %q but its challenge named %q", ar.AgentID, ch.AgentID)
			}
		}
	}

	if raw, ok := frames["AUTH_OK"]; !ok {
		t.Errorf("the README mesh example must quote an AUTH_OK frame — it is how a client knows it was admitted")
	} else {
		var okMsg AuthOK
		decodeStrict(t, raw, &okMsg)
		assertFrame(t, "AUTH_OK", func() []string {
			var bad []string
			if okMsg.Type != TypeAuthOK {
				bad = append(bad, fmt.Sprintf("type=%q", okMsg.Type))
			}
			if okMsg.AgentID == "" {
				bad = append(bad, "agent_id is empty")
			}
			return bad
		})
	}

	// The wrong-type direction, so "parses into its documented type" means
	// something: the RESPONSE's correlation field is exactly what a frame
	// without one cannot supply.
	var asKeepalive Keepalive
	dec := json.NewDecoder(bytes.NewReader(respRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&asKeepalive); err == nil {
		t.Error("the README's RESPONSE frame also decodes cleanly as a KEEPALIVE — the two documented shapes are not distinguishable")
	}
}

// assertFrame reports every missing/wrong field at once, naming the frame.
func assertFrame(t *testing.T, name string, check func() []string) {
	t.Helper()
	if bad := check(); len(bad) > 0 {
		t.Errorf("the README's %s frame does not carry the fields the section documents: %s", name, strings.Join(bad, "; "))
	}
}

// TestReadmeStatesTheCorrelationAndKeepaliveRules keeps the two normative
// sentences alive. They are the whole point of the section — a reader can write
// a client from them — so deleting or rewording one fails the build instead of
// shipping a doc that only shows a frame.
func TestReadmeStatesTheCorrelationAndKeepaliveRules(t *testing.T) {
	doc := readme(t)

	for _, want := range []string{
		// The correlation rule, in the doc's own words.
		"Correlate the reply on `request_id`",
		"MUST set `request_id` to the exact\n`message_id` of the REQUEST it is answering",
		// The KEEPALIVE rule a client must implement.
		"Read in a loop and dispatch on `type`",
		"ignore `KEEPALIVE` frames while a reply",
		// The zero-install path, so the section stays runnable without a web client.
		"bash examples/ws-mesh-demo/run-demo.sh",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("README.md no longer states %q — the mesh section must keep it", want)
		}
	}

	// The documented keepalive interval must be the shipped one. If
	// DefaultMeshConfig moves, this fails instead of leaving the README
	// describing a cadence the server does not use.
	secs := int(DefaultMeshConfig("").KeepaliveInterval.Seconds())
	if secs != 30 {
		t.Logf("keepalive interval is now %ds (was 30s when this gate was written)", secs)
	}
	for _, want := range []string{
		fmt.Sprintf("every\n%d seconds", secs),
		fmt.Sprintf("KeepaliveInterval = %d * time.Second", secs),
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("README.md does not quote the shipped keepalive interval (%q)", want)
		}
	}
	if DefaultMeshConfig("").KeepaliveInterval != 30*time.Second {
		t.Errorf("DefaultMeshConfig keepalive interval = %s, but the README's mesh example is written against 30s — update both",
			DefaultMeshConfig("").KeepaliveInterval)
	}
}

// TestReadmeMeshFramesCoverTheDocumentedTypes makes the frame set explicit: the
// example must show the exchange (REQUEST + RESPONSE) and the frames a client
// meets around it, and it must not quietly lose one.
func TestReadmeMeshFramesCoverTheDocumentedTypes(t *testing.T) {
	frames := readmeMeshFrames(t)
	for _, want := range []string{"REGISTER", "REQUEST", "KEEPALIVE", "RESPONSE", "ERROR",
		"AUTH_CHALLENGE", "AUTH_RESPONSE", "AUTH_OK"} {
		if _, ok := frames[want]; !ok {
			t.Errorf("the README mesh example no longer quotes a %s frame", want)
		}
	}
	// Every message type the package defines must be handled: either quoted in
	// the README (and therefore decoded above) or deliberately absent. The one
	// deliberate absence is REGISTER_ACK — the type exists but no server code
	// path emits it (docs/mesh-protocol.md §REGISTER_ACK). The AUTH_* frames are
	// quoted even though they only appear on an authenticated mesh, because a
	// client meeting one has to know its shape before it can answer.
	if _, quoted := frames[string(TypeRegisterAck)]; quoted {
		t.Error("the README quotes a REGISTER_ACK frame, which no server code path emits — the doc would teach a handshake that never happens")
	}
}

// TestMeshFrameExtractionRefusesEmptyAndBrokenRegions proves the gate cannot be
// satisfied by reading nothing. It drives extractMeshFrames on synthetic docs,
// so it holds regardless of what README.md says today — including the case the
// ticket calls out: a marked region with no frame in it must FAIL, not pass.
func TestMeshFrameExtractionRefusesEmptyAndBrokenRegions(t *testing.T) {
	const frame = `{"type":"REQUEST","version":1,"message_id":"9d8f0a1b2c3d4e5f6a7b8c9d","timestamp":"2026-09-18T09:15:01.123456789-05:00","source":{"agent_id":"a"},"target":{"agent_id":"b"},"method":"GET","path":"/ping","trace_id":"f1e2d3c4b5a69788796a5b4c","timeout_ms":5000}`

	cases := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"no marker at all", "### Try the Mesh\n" + frame + "\n", "no <!-- mesh-frames:start -->"},
		{"marker opened, never closed", readmeFramesStart + "\n" + frame + "\n", "never closes"},
		{"marked region with no frame", readmeFramesStart + "\n```json\n```\n" + readmeFramesEnd, "contains no JSON frame"},
		{"marked region of prose only", readmeFramesStart + "\nsend a REQUEST\n" + readmeFramesEnd, "contains no JSON frame"},
		{"frame that is not JSON", readmeFramesStart + "\n{not json\n" + readmeFramesEnd, "not valid JSON"},
		{"frame with no type", readmeFramesStart + "\n{\"version\":1}\n" + readmeFramesEnd, "carries no `type`"},
		{"the same type twice", readmeFramesStart + "\n" + frame + "\n" + frame + "\n" + readmeFramesEnd, "more than one REQUEST frame"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractMeshFrames(tc.doc)
			if err == nil {
				t.Fatalf("extractMeshFrames accepted a doc it must refuse (got %d frame(s)): %q", len(got), tc.doc)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("extractMeshFrames error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	// ...and the positive control, or the refusals above would be consistent
	// with an extractor that refuses everything.
	frames, err := extractMeshFrames(readmeFramesStart + "\n```json\n" + frame + "\n```\n" + readmeFramesEnd)
	if err != nil {
		t.Fatalf("extractMeshFrames refused a well-formed marked region: %v", err)
	}
	if len(frames) != 1 || frames["REQUEST"] == nil {
		t.Fatalf("extractMeshFrames returned %d frame(s), want exactly the REQUEST", len(frames))
	}
}
