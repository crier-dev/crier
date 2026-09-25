package main

// mesh_auth_test.go — DF-CRIER-287, the end-to-end half: the LIVE server, over
// its real router and real registry, refuses a mesh peer it cannot authenticate
// and admits one it can.
//
// The in-package tests in internal/mesh prove the handshake against a stub key
// source. This file proves the wiring that makes it matter: the key the mesh
// verifies against is the one stored by POST /agents, the refusal happens on a
// real listening server, GET /mesh/peers never lists an unauthenticated
// connection, GET /status reports the posture, and the mesh's own origin
// allowlist answers 403 for a browser origin it was not told about while an
// agent (no Origin header) still connects.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// registerMeshAgent registers an agent with a real ed25519 public key and
// returns the private half, so the test can sign as that identity.
func registerMeshAgent(t *testing.T, base, agentID string) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"id":           agentID,
		"public_key":   hex.EncodeToString(pub),
		"capabilities": []string{},
	})
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	resp, err := http.Post(base+"/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /agents returned %d: %s", resp.StatusCode, raw)
	}
	return priv
}

// meshPeers reads GET /mesh/peers and returns the listed agent ids.
func meshPeers(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/mesh/peers")
	if err != nil {
		t.Fatalf("GET /mesh/peers: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Peers []struct {
			AgentID string `json:"agent_id"`
		} `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode GET /mesh/peers: %v", err)
	}
	ids := make([]string, 0, len(body.Peers))
	for _, p := range body.Peers {
		ids = append(ids, p.AgentID)
	}
	return ids
}

// answerMeshChallenge completes the connect handshake as agentID, signing the
// challenge with priv. It returns the type of the frame that followed, plus the
// raw ERROR message when that frame was a refusal.
func answerMeshChallenge(t *testing.T, conn *websocket.Conn, agentID string, priv ed25519.PrivateKey, timeout time.Duration) (string, string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no AUTH_CHALLENGE arrived: %v", err)
	}
	var probe struct {
		Type      string `json:"type"`
		Nonce     string `json:"nonce"`
		AgentID   string `json:"agent_id"`
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("first frame is not JSON: %v (%s)", err, data)
	}
	if probe.Type != "AUTH_CHALLENGE" {
		return probe.Type, string(data)
	}
	if probe.AgentID != agentID {
		t.Fatalf("challenge names %q, want %q", probe.AgentID, agentID)
	}

	// A nil key stands for "this client cannot authenticate": it answers with
	// the frame a pre-fix client would send first (its REGISTER), which is
	// exactly what a server that now requires the handshake has to refuse.
	if priv == nil {
		old, err := json.Marshal(map[string]any{
			"type":         "REGISTER",
			"version":      1,
			"message_id":   "old-client-" + probe.MessageID,
			"timestamp":    time.Now().Format(time.RFC3339Nano),
			"agent_id":     agentID,
			"lease_id":     "",
			"lease_ttl_ms": 3600000,
			"capabilities": map[string]any{"version": "0.1.0", "topics": []string{}, "max_concurrent_sessions": 1},
		})
		if err != nil {
			t.Fatalf("marshal REGISTER: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, append(old, '\n')); err != nil {
			t.Fatalf("write REGISTER: %v", err)
		}
	} else {
		payload := []byte("mesh-auth-v1\n" + agentID + "\n" + probe.Nonce)
		resp := map[string]any{
			"type":       "AUTH_RESPONSE",
			"version":    1,
			"message_id": "probe-" + probe.MessageID,
			"timestamp":  time.Now().Format(time.RFC3339Nano),
			"agent_id":   agentID,
			"nonce":      probe.Nonce,
			"signature":  hex.EncodeToString(ed25519.Sign(priv, payload)),
		}
		out, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal AUTH_RESPONSE: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, append(out, '\n')); err != nil {
			t.Fatalf("write AUTH_RESPONSE: %v", err)
		}
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("no reply to AUTH_RESPONSE: %v", err)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("reply is not JSON: %v (%s)", err, data)
	}
	return env.Type, env.Error.Message
}

// TestMeshAuthLiveServerRefusesUnauthenticatedPeer is the acceptance case on a
// live server: a client that connects as a registered agent without proving its
// key is refused, is closed, and is never listed as a peer.
func TestMeshAuthLiveServerRefusesUnauthenticatedPeer(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":     "false",
		"CR_REQUIRE_MESH_AUTH": "true",
	})
	const agentID = "mesh-auth-live-unsigned"
	registerMeshAgent(t, base, agentID)

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	type_, reason := answerMeshChallenge(t, conn, agentID, nil, 3*time.Second)
	if type_ != "ERROR" {
		t.Fatalf("a client that cannot authenticate got %q, want an ERROR refusal (%s)", type_, reason)
	}
	if !strings.Contains(reason, "AUTH_RESPONSE") {
		t.Errorf("refusal message = %q, want it to name the AUTH_RESPONSE frame that was required", reason)
	}
	if ids := meshPeers(t, base); len(ids) != 0 {
		t.Fatalf("GET /mesh/peers listed %v — an unauthenticated connection must never become a peer", ids)
	}
}

// TestMeshAuthLiveServerAdmitsSignedPeer: the same live server admits a peer
// that signs with the key POST /agents stored for it.
func TestMeshAuthLiveServerAdmitsSignedPeer(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":     "false",
		"CR_REQUIRE_MESH_AUTH": "true",
	})
	const agentID = "mesh-auth-live-signed"
	priv := registerMeshAgent(t, base, agentID)

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	type_, reason := answerMeshChallenge(t, conn, agentID, priv, 3*time.Second)
	if type_ != "AUTH_OK" {
		t.Fatalf("frame after a correct signature = %q, want AUTH_OK (%s)", type_, reason)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		ids := meshPeers(t, base)
		for _, id := range ids {
			if id == agentID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /mesh/peers never listed the authenticated peer (last: %v)", ids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMeshAuthLiveServerRefusesImpostorKey: a peer holding a DIFFERENT valid
// key cannot take over a registered identity — the impersonation the row is
// about, refused on a live server.
func TestMeshAuthLiveServerRefusesImpostorKey(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":     "false",
		"CR_REQUIRE_MESH_AUTH": "true",
	})
	const agentID = "mesh-auth-live-victim"
	registerMeshAgent(t, base, agentID) // registers the victim's real key
	_, impostorKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate impostor key: %v", err)
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	type_, reason := answerMeshChallenge(t, conn, agentID, impostorKey, 3*time.Second)
	if type_ != "ERROR" {
		t.Fatalf("an impostor's frame = %q, want an ERROR refusal (%s)", type_, reason)
	}
	if !strings.Contains(reason, "signature verification failed") {
		t.Errorf("refusal message = %q, want it to say the signature did not verify", reason)
	}
	if ids := meshPeers(t, base); len(ids) != 0 {
		t.Fatalf("the impostor was admitted: GET /mesh/peers listed %v", ids)
	}
}

// TestMeshAuthPostureReportedByStatus: an operator auditing the live server sees
// the mesh posture rather than having to infer it from a boot log line.
func TestMeshAuthPostureReportedByStatus(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":        "false",
		"CR_REQUIRE_MESH_AUTH":    "true",
		"CR_MESH_ALLOWED_ORIGINS": "https://console.example.com",
	})

	resp, err := http.Get(base + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	if body["mesh_auth_required"] != true {
		t.Errorf("mesh_auth_required = %#v, want true (CR_REQUIRE_MESH_AUTH=true)", body["mesh_auth_required"])
	}
	if body["mesh_origin_policy"] != "allowlist" {
		t.Errorf("mesh_origin_policy = %#v, want \"allowlist\"", body["mesh_origin_policy"])
	}
}

// TestMeshOriginAllowlistLive: the mesh's own origin policy, live. With an
// allowlist set, a browser origin that is not listed is refused (403 at the
// upgrade) while a client with NO Origin header — every shipped agent client —
// still connects. The default (no allowlist) stays allow-all, which the auth-off
// boots above already exercise.
func TestMeshOriginAllowlistLive(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":        "false",
		"CR_MESH_ALLOWED_ORIGINS": "https://console.example.com",
	})
	const agentID = "mesh-origin-live"
	registerMeshAgent(t, base, agentID)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID

	// 1. An agent (no Origin header) connects: an allowlist must not lock out
	// the non-browser clients the lane exists for.
	agent, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("an agent with no Origin header was refused under an allowlist: %v", err)
	}
	agent.Close()

	// 2. A browser origin that is not on the list is refused before any frame.
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"https://evil.example"}})
	if err == nil {
		t.Fatal("a mesh socket was upgraded from an unlisted Origin")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("unlisted-Origin upgrade answered %d, want 403", got)
	}

	// 3. The listed origin is accepted.
	ok, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"https://console.example.com"}})
	if err != nil {
		t.Fatalf("the listed Origin was refused: %v", err)
	}
	ok.Close()
}

// TestMeshAuthDefaultsOffLive is the no-regression case on a live server: with
// the flag unset the old behaviour stands — a raw socket with no handshake at
// all is a peer, exactly as before this row.
func TestMeshAuthDefaultsOffLive(t *testing.T) {
	base := bootStatusServer(t, map[string]string{"CR_GUARD_ENABLED": "false"})
	const agentID = "mesh-default-live"

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		ids := meshPeers(t, base)
		for _, id := range ids {
			if id == agentID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("a default (auth-off) server no longer admits a plain connection —/mesh/peers: %v", ids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMeshAuthTimeoutConfigurableLive proves CR_MESH_AUTH_TIMEOUT_S reaches the
// live handshake: with a 1s budget, a silent client is refused by that deadline
// and told so, rather than sitting on a pre-auth socket.
func TestMeshAuthTimeoutConfigurableLive(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":       "false",
		"CR_REQUIRE_MESH_AUTH":   "true",
		"CR_MESH_AUTH_TIMEOUT_S": "1",
	})
	const agentID = "mesh-auth-timeout-live"
	registerMeshAgent(t, base, agentID)

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	start := time.Now()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("no refusal arrived for a silent client: %v", err)
		}
		var env struct {
			Type  string `json:"type"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("frame is not JSON: %v (%s)", err, data)
		}
		if env.Type != "ERROR" {
			continue // the AUTH_CHALLENGE
		}
		if env.Error.Code != "AUTH_FAILED" {
			t.Fatalf("refusal code = %q, want AUTH_FAILED (%s)", env.Error.Code, env.Error.Message)
		}
		if !strings.Contains(env.Error.Message, "no AUTH_RESPONSE within 1s") {
			t.Errorf("refusal message = %q, want it to name the configured 1s window", env.Error.Message)
		}
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Errorf("refusal took %v, want it bounded by the configured 1s window (+ slack)", elapsed)
		}
		return
	}
}

// TestMeshAuthLiveRequestSourceCannotBeSpoofed is the frame-level half, live:
// two authenticated peers, one of which names the other as its REQUEST source.
// The server refuses it FORBIDDEN and the target never sees the frame.
func TestMeshAuthLiveRequestSourceCannotBeSpoofed(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":     "false",
		"CR_REQUIRE_MESH_AUTH": "true",
	})
	const victimID = "mesh-live-victim"
	const attackerID = "mesh-live-attacker"
	const targetID = "mesh-live-target"
	registerMeshAgent(t, base, victimID)
	attackerKey := registerMeshAgent(t, base, attackerID)
	targetKey := registerMeshAgent(t, base, targetID)

	wsURL := func(id string) string {
		return "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + id
	}

	attacker, _, err := websocket.DefaultDialer.Dial(wsURL(attackerID), nil)
	if err != nil {
		t.Fatalf("dial attacker: %v", err)
	}
	defer attacker.Close()
	if type_, reason := answerMeshChallenge(t, attacker, attackerID, attackerKey, 3*time.Second); type_ != "AUTH_OK" {
		t.Fatalf("attacker handshake = %q, want AUTH_OK (%s)", type_, reason)
	}

	target, _, err := websocket.DefaultDialer.Dial(wsURL(targetID), nil)
	if err != nil {
		t.Fatalf("dial target: %v", err)
	}
	defer target.Close()
	if type_, reason := answerMeshChallenge(t, target, targetID, targetKey, 3*time.Second); type_ != "AUTH_OK" {
		t.Fatalf("target handshake = %q, want AUTH_OK (%s)", type_, reason)
	}

	// The attacker asks the target to do something AS the victim.
	req, err := json.Marshal(map[string]any{
		"type":       "REQUEST",
		"version":    1,
		"message_id": "spoof-live-1",
		"timestamp":  time.Now().Format(time.RFC3339Nano),
		"source":     map[string]string{"agent_id": victimID},
		"target":     map[string]string{"agent_id": targetID},
		"method":     "POST",
		"path":       "/privileged",
		"trace_id":   "spoof-live-1-trace",
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("marshal REQUEST: %v", err)
	}
	if err := attacker.WriteMessage(websocket.TextMessage, append(req, '\n')); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}

	// The attacker is refused...
	if err := attacker.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := attacker.ReadMessage()
	if err != nil {
		t.Fatalf("no refusal for a spoofed source: %v", err)
	}
	var refusal struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Error     struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &refusal); err != nil {
		t.Fatalf("refusal is not JSON: %v (%s)", err, data)
	}
	if refusal.Type != "ERROR" || refusal.Error.Code != "FORBIDDEN" {
		t.Fatalf("refusal = %s %s %q, want ERROR FORBIDDEN", refusal.Type, refusal.Error.Code, refusal.Error.Message)
	}
	if refusal.RequestID != "spoof-live-1" {
		t.Errorf("refusal request_id = %q, want the refused REQUEST's message_id", refusal.RequestID)
	}

	// ...and the target never sees it.
	if err := target.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, data, err := target.ReadMessage(); err == nil {
		t.Fatalf("the target received a REQUEST that named another agent as its source: %s", data)
	}
}

// TestMeshAuthLiveServerRefusesUnregisteredAgent covers the live registry
// adapter's failure branch: an agent id with no registry row (or a row without a
// usable key) cannot be verified, so the connect is refused and the message
// names the missing registration rather than blaming a signature.
func TestMeshAuthLiveServerRefusesUnregisteredAgent(t *testing.T) {
	base := bootStatusServer(t, map[string]string{
		"CR_GUARD_ENABLED":     "false",
		"CR_REQUIRE_MESH_AUTH": "true",
	})
	const agentID = "mesh-auth-live-never-registered"

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/mesh/connect/"+agentID, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no refusal for an unregistered agent: %v", err)
	}
	var refusal struct {
		Type  string `json:"type"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &refusal); err != nil {
		t.Fatalf("frame is not JSON: %v (%s)", err, data)
	}
	if refusal.Type != "ERROR" || refusal.Error.Code != "AUTH_FAILED" {
		t.Fatalf("refusal = %s %s %q, want ERROR AUTH_FAILED", refusal.Type, refusal.Error.Code, refusal.Error.Message)
	}
	if !strings.Contains(refusal.Error.Message, "no registered ed25519 public key") {
		t.Errorf("refusal message = %q, want it to name the missing registration", refusal.Error.Message)
	}
	if ids := meshPeers(t, base); len(ids) != 0 {
		t.Fatalf("GET /mesh/peers listed %v — an unverifiable agent must never become a peer", ids)
	}
}
