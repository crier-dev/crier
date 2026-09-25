package mesh

// auth_test.go — DF-CRIER-287: the connect handshake and the identity rules it
// makes possible.
//
// The row this file closes was filed from an external review whose evidence was
// a grep: `grep -rnE 'requireAgent|Signature|token|auth' internal/mesh/*.go`
// returned ZERO hits, so `/mesh/connect/{agentID}` proved nothing about the
// caller while the registry lane enforced an ed25519 signature per agent. The
// tests below are the acceptance evidence in-repo: a peer that cannot sign for
// the identity in the URL path is refused and never becomes a peer, a peer that
// can is admitted, and an admitted peer cannot speak as another agent on the
// frames it sends (the impersonation that made the missing handshake matter).
//
// Everything is opt-in, so the LAST test here pins the default: with
// MeshAuthConfig.Required unset, a socket with no handshake at all still
// becomes a peer — the behaviour every existing client depends on.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// expectNoFrame asserts nothing readable arrives within wait: a frame fails the
// test, a read timeout or a clean close passes. It is how the frame-rule tests
// state "the target never saw it" without sleeping on a fixed budget.
func expectNoFrame(t *testing.T, conn *websocket.Conn, wait time.Duration, why string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("%s: received %s", why, data)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return
	}
	t.Fatalf("%s: read failed with %v", why, err)
}

// authTestKeyPair returns a fresh ed25519 keypair as hex, so a test can sign as
// an agent without touching key files.
func authTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

// staticKeyProvider answers with one registered key for one agent id, and fails
// every other lookup — the shape a registry-backed provider has for a single
// registered agent.
type staticKeyProvider struct {
	agentID string
	pub     ed25519.PublicKey
	err     error
}

func (p staticKeyProvider) RegisteredPublicKey(agentID string) (ed25519.PublicKey, error) {
	if p.err != nil {
		return nil, p.err
	}
	if agentID != p.agentID {
		return nil, fmt.Errorf("agent %q not found", agentID)
	}
	return p.pub, nil
}

// authTestServer boots a mesh accept path that REQUIRES the handshake, over the
// real HandleConnect, and returns the mesh, its base URL and the ws URL for
// agentID.
func authTestServer(t *testing.T, agentID string, pub ed25519.PublicKey, timeout time.Duration) (*Mesh, string, string) {
	t.Helper()
	cfg := DefaultMeshConfig("crier-under-test")
	cfg.KeepaliveInterval = time.Hour // never tick during a test
	cfg.Auth = MeshAuthConfig{Required: true, Timeout: timeout}
	m := NewMesh(cfg)
	m.SetAgentKeyProvider(staticKeyProvider{agentID: agentID, pub: pub})

	router := mux.NewRouter()
	router.HandleFunc("/mesh/connect/{agentID}", HandleConnect(m)).Methods("GET")
	router.HandleFunc("/mesh/peers", HandlePeers(m)).Methods("GET")
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	t.Cleanup(m.Stop)

	base := server.URL
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/" + agentID
	return m, base, wsURL
}

// readFrame reads one frame with a deadline and decodes its envelope.
func readFrame(t *testing.T, conn *websocket.Conn, wait time.Duration) (Envelope, []byte) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame (waited %s): %v", wait, err)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("frame is not a JSON envelope: %v (%s)", err, data)
	}
	return env, data
}

// readErrorMessage reads until it sees an ERROR frame, failing the test if the
// socket closes first — a refusal that never arrives is not a refusal.
func readErrorMessage(t *testing.T, conn *websocket.Conn, wait time.Duration) ErrorMessage {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("expected an ERROR frame within %s, got a read failure instead: %v", wait, err)
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("frame is not a JSON envelope: %v (%s)", err, data)
		}
		if env.Type != TypeError {
			continue
		}
		var errMsg ErrorMessage
		if err := json.Unmarshal(data, &errMsg); err != nil {
			t.Fatalf("ERROR frame does not decode: %v (%s)", err, data)
		}
		if errMsg.Error.Code == "" || errMsg.Error.Message == "" {
			t.Fatalf("ERROR frame carries no code/message: %s", data)
		}
		return errMsg
	}
}

// peerIDs polls GET /mesh/peers until it lists want (or the deadline passes).
func peerIDs(t *testing.T, base string) []string {
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
		t.Fatalf("decode /mesh/peers: %v", err)
	}
	ids := make([]string, 0, len(body.Peers))
	for _, p := range body.Peers {
		ids = append(ids, p.AgentID)
	}
	return ids
}

func waitForPeers(t *testing.T, base, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var ids []string
	for time.Now().Before(deadline) {
		ids = peerIDs(t, base)
		for _, id := range ids {
			if id == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET /mesh/peers never listed %q within %s (last: %v)", want, timeout, ids)
}

// TestMeshAuthPayloadIsPinnedToItsBytes is the cross-implementation contract:
// the payload a client signs is NOT self-describing on the wire, so its exact
// bytes are the spec. A reimplementation (the Python worked example in
// docs/mesh-protocol.md, or any other language) signs this string; if the shape
// here drifts, that client silently stops authenticating.
func TestMeshAuthPayloadIsPinnedToItsBytes(t *testing.T) {
	got := string(MeshAuthPayload("agent-a", "0123456789abcdef0123456789abcdef"))
	want := "mesh-auth-v1\nagent-a\n0123456789abcdef0123456789abcdef"
	if got != want {
		t.Fatalf("MeshAuthPayload = %q, want %q (the documented payload; a client signing the documented bytes must verify)", got, want)
	}

	// Domain separation: the same signature must not be usable as an inbox
	// signature (which covers "<METHOD>\n<path>\n<unix-seconds>") or vice
	// versa. Different prefixes at minimum.
	if strings.HasPrefix(want, "GET\n") || strings.HasPrefix(want, "POST\n") {
		t.Fatal("the mesh payload must not share the inbox signature's shape")
	}
}

// TestMeshAuthRefusesUnsignedPeer is the headline acceptance case: a client
// that connects as an agent and sends nothing (or cannot sign) is refused with
// AUTH_FAILED, is closed, and NEVER appears in GET /mesh/peers.
func TestMeshAuthRefusesUnsignedPeer(t *testing.T) {
	pub, _ := authTestKeyPair(t)
	m, base, wsURL := authTestServer(t, "agent-a", pub, 2*time.Second)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The server challenges first; a peer that cannot answer is refused.
	env, data := readFrame(t, conn, 2*time.Second)
	if env.Type != TypeAuthChallenge {
		t.Fatalf("first frame type = %q, want AUTH_CHALLENGE (%s)", env.Type, data)
	}
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatalf("AUTH_CHALLENGE does not decode: %v", err)
	}
	if challenge.AgentID != "agent-a" {
		t.Errorf("challenge agent_id = %q, want agent-a (the path claim under test)", challenge.AgentID)
	}
	if len(challenge.Nonce) != 32 {
		t.Errorf("challenge nonce = %q, want 32 hex chars", challenge.Nonce)
	}
	if isHex := func(s string) bool {
		_, err := hex.DecodeString(s)
		return err == nil
	}; !isHex(challenge.Nonce) {
		t.Errorf("challenge nonce %q is not hex", challenge.Nonce)
	}
	if challenge.ExpiresAt.IsZero() {
		t.Error("challenge carries no expires_at")
	}

	// Send an intentionally unauthenticated frame: a REGISTER is refused, and
	// the refusal names the handshake rather than routing the frame.
	reg := &Register{
		Envelope: Envelope{Type: TypeRegister, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:  "agent-a",
	}
	data, err = Marshal(reg)
	if err != nil {
		t.Fatalf("marshal REGISTER: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write REGISTER: %v", err)
	}
	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed {
		t.Errorf("refusal code = %q, want %q", errMsg.Error.Code, ErrCodeAuthFailed)
	}
	if !strings.Contains(errMsg.Error.Message, "AUTH_RESPONSE") {
		t.Errorf("refusal message %q should name the frame that was required", errMsg.Error.Message)
	}

	if ids := peerIDs(t, base); len(ids) != 0 {
		t.Errorf("GET /mesh/peers listed %v — an unauthenticated connection must never become a peer", ids)
	}
	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers = %d, want 0", m.ActivePeers())
	}
}

// TestMeshAuthAdmitsSignedPeer is the other half of the acceptance evidence: a
// peer that holds the registered private key answers the challenge and IS
// admitted, with AUTH_OK sent before any other frame.
func TestMeshAuthAdmitsSignedPeer(t *testing.T) {
	pub, priv := authTestKeyPair(t)
	m, base, wsURL := authTestServer(t, "agent-a", pub, 5*time.Second)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	env, data := readFrame(t, conn, 2*time.Second)
	if env.Type != TypeAuthChallenge {
		t.Fatalf("first frame = %q, want AUTH_CHALLENGE", env.Type)
	}
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	resp := &AuthResponse{
		Envelope: Envelope{Type: TypeAuthResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:  "agent-a",
		Nonce:    challenge.Nonce,
		Signature: hex.EncodeToString(
			ed25519.Sign(priv, MeshAuthPayload("agent-a", challenge.Nonce))),
	}
	out, err := Marshal(resp)
	if err != nil {
		t.Fatalf("marshal AUTH_RESPONSE: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write AUTH_RESPONSE: %v", err)
	}

	env, data = readFrame(t, conn, 2*time.Second)
	if env.Type != TypeAuthOK {
		t.Fatalf("frame after the correct signature = %q, want AUTH_OK (%s)", env.Type, data)
	}
	var ok AuthOK
	if err := json.Unmarshal(data, &ok); err != nil {
		t.Fatalf("decode AUTH_OK: %v", err)
	}
	if ok.AgentID != "agent-a" {
		t.Errorf("AUTH_OK agent_id = %q, want agent-a", ok.AgentID)
	}

	waitForPeers(t, base, "agent-a", 2*time.Second)
	if m.ActivePeers() != 1 {
		t.Errorf("ActivePeers = %d, want 1", m.ActivePeers())
	}
}

// TestMeshAuthRefusesWrongKey: the signature is well-formed and the nonce is
// right, but the key is not the one registered for that id — the impersonation
// the row is about, refused.
func TestMeshAuthRefusesWrongKey(t *testing.T) {
	pub, _ := authTestKeyPair(t)
	_, attackerPriv := authTestKeyPair(t)
	_, base, wsURL := authTestServer(t, "agent-a", pub, 2*time.Second)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, data := readFrame(t, conn, 2*time.Second)
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	resp := &AuthResponse{
		Envelope: Envelope{Type: TypeAuthResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:  "agent-a",
		Nonce:    challenge.Nonce,
		Signature: hex.EncodeToString(
			ed25519.Sign(attackerPriv, MeshAuthPayload("agent-a", challenge.Nonce))),
	}
	out, _ := Marshal(resp)
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write AUTH_RESPONSE: %v", err)
	}

	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed {
		t.Fatalf("refusal code = %q, want %q (%s)", errMsg.Error.Code, ErrCodeAuthFailed, errMsg.Error.Message)
	}
	if !strings.Contains(errMsg.Error.Message, "signature verification failed") {
		t.Errorf("refusal message %q should say the signature did not verify", errMsg.Error.Message)
	}
	if ids := peerIDs(t, base); len(ids) != 0 {
		t.Errorf("an impostor was admitted: /mesh/peers listed %v", ids)
	}
}

// TestMeshAuthRefusesWrongAgentAndNonce covers the two other ways a
// well-signed answer can still be the wrong answer: it names a different agent
// id, or it echoes a nonce that is not this challenge's (a replayed or
// fabricated response).
func TestMeshAuthRefusesWrongAgentAndNonce(t *testing.T) {
	pub, priv := authTestKeyPair(t)
	_, base, wsURL := authTestServer(t, "agent-a", pub, 2*time.Second)

	cases := []struct {
		name    string
		mutate  func(*AuthResponse, AuthChallenge)
		wantSub string
	}{
		{
			name:   "names another agent id",
			mutate: func(r *AuthResponse, _ AuthChallenge) { r.AgentID = "agent-b" },
			// The signature is over the CLAIMED id, so the server checks the
			// id first: a response for another identity is not a signature
			// failure, it is a refusal to switch identity mid-connection.
			wantSub: "may only authenticate as itself",
		},
		{
			name: "echoes a foreign nonce",
			mutate: func(r *AuthResponse, _ AuthChallenge) {
				// Signed over the foreign nonce, so the signature itself is
				// valid — only the nonce check can catch this.
				r.Nonce = "ffffffffffffffffffffffffffffffff"
				r.Signature = hex.EncodeToString(ed25519.Sign(priv, MeshAuthPayload("agent-a", r.Nonce)))
			},
			wantSub: "nonce does not match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			_, data := readFrame(t, conn, 2*time.Second)
			var challenge AuthChallenge
			if err := json.Unmarshal(data, &challenge); err != nil {
				t.Fatalf("decode challenge: %v", err)
			}
			resp := &AuthResponse{
				Envelope: Envelope{Type: TypeAuthResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
				AgentID:  "agent-a",
				Nonce:    challenge.Nonce,
				Signature: hex.EncodeToString(
					ed25519.Sign(priv, MeshAuthPayload("agent-a", challenge.Nonce))),
			}
			tc.mutate(resp, challenge)

			out, _ := Marshal(resp)
			if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
				t.Fatalf("write AUTH_RESPONSE: %v", err)
			}
			errMsg := readErrorMessage(t, conn, 2*time.Second)
			if errMsg.Error.Code != ErrCodeAuthFailed {
				t.Fatalf("refusal code = %q, want %q (%s)", errMsg.Error.Code, ErrCodeAuthFailed, errMsg.Error.Message)
			}
			if !strings.Contains(errMsg.Error.Message, tc.wantSub) {
				t.Errorf("refusal message = %q, want it to mention %q", errMsg.Error.Message, tc.wantSub)
			}
		})
	}

	if ids := peerIDs(t, base); len(ids) != 0 {
		t.Errorf("a refused connection became a peer: /mesh/peers listed %v", ids)
	}
}

// TestMeshAuthRefusesUnregisteredAgent: no registry row (or a row without a
// usable key) cannot be verified, so the connect fails closed and the message
// names the remedy rather than blaming the caller's signature.
func TestMeshAuthRefusesUnregisteredAgent(t *testing.T) {
	pub, _ := authTestKeyPair(t)
	_, base, wsURL := authTestServer(t, "agent-a", pub, 2*time.Second)

	// Connect as a DIFFERENT id: the provider only knows agent-a.
	unknownURL := strings.Replace(wsURL, "/agent-a", "/agent-nobody", 1)
	conn, _, err := websocket.DefaultDialer.Dial(unknownURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed {
		t.Fatalf("refusal code = %q, want %q", errMsg.Error.Code, ErrCodeAuthFailed)
	}
	if !strings.Contains(errMsg.Error.Message, "no registered ed25519 public key") {
		t.Errorf("refusal message = %q, want it to name the missing registration", errMsg.Error.Message)
	}
	if ids := peerIDs(t, base); len(ids) != 0 {
		t.Errorf("an unverifiable agent was admitted: /mesh/peers listed %v", ids)
	}
}

// TestMeshAuthTimesOutSilentPeer: a peer that receives the challenge and says
// nothing is refused by the deadline AND told why, rather than parked forever.
func TestMeshAuthTimesOutSilentPeer(t *testing.T) {
	pub, _ := authTestKeyPair(t)
	_, base, wsURL := authTestServer(t, "agent-a", pub, 300*time.Millisecond)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if env, _ := readFrame(t, conn, 2*time.Second); env.Type != TypeAuthChallenge {
		t.Fatalf("first frame = %q, want AUTH_CHALLENGE", env.Type)
	}
	// Say nothing. The refusal must arrive on its own.
	errMsg := readErrorMessage(t, conn, 3*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed {
		t.Fatalf("refusal code = %q, want %q", errMsg.Error.Code, ErrCodeAuthFailed)
	}
	if !strings.Contains(errMsg.Error.Message, "no AUTH_RESPONSE within") {
		t.Errorf("refusal message = %q, want it to name the expired challenge window", errMsg.Error.Message)
	}
	if ids := peerIDs(t, base); len(ids) != 0 {
		t.Errorf("a silent peer was admitted: /mesh/peers listed %v", ids)
	}
}

// TestMeshAuthMalformedResponseIsRefusedAndRecoverable: one bad frame is a
// refusal, not a dead socket — the handshake stays open for a correct answer
// within the same window (the same "the connection stays usable" contract the
// rest of the mesh keeps for malformed frames).
func TestMeshAuthMalformedResponseIsRefusedAndRecoverable(t *testing.T) {
	pub, priv := authTestKeyPair(t)
	_, base, wsURL := authTestServer(t, "agent-a", pub, 5*time.Second)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, data := readFrame(t, conn, 2*time.Second)
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	// 1. Not JSON at all -> INVALID_MESSAGE, with no request_id to correlate to.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("not json\n")); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeInvalidMessage {
		t.Errorf("junk frame refusal code = %q, want %q", errMsg.Error.Code, ErrCodeInvalidMessage)
	}
	if errMsg.RequestID != "" {
		t.Errorf("refusal for an unreadable frame carries request_id %q, want it absent", errMsg.RequestID)
	}

	// 2. A correct answer on the SAME socket is still accepted: one unreadable
	// frame is a refusal, not a dead handshake.
	good := &AuthResponse{
		Envelope:  Envelope{Type: TypeAuthResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:   "agent-a",
		Nonce:     challenge.Nonce,
		Signature: hex.EncodeToString(ed25519.Sign(priv, MeshAuthPayload("agent-a", challenge.Nonce))),
	}
	out, _ := Marshal(good)
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write good signature: %v", err)
	}
	if env, data := readFrame(t, conn, 2*time.Second); env.Type != TypeAuthOK {
		t.Fatalf("frame after recovering = %q, want AUTH_OK (%s)", env.Type, data)
	}
	waitForPeers(t, base, "agent-a", 2*time.Second)
}

// TestMeshAuthMalformedSignatureIsFatal: a present-but-unusable signature is
// the end of the handshake — the socket is refused and closed, so a client that
// cannot produce a signature cannot sit on a pre-auth socket retrying shapes.
func TestMeshAuthMalformedSignatureIsFatal(t *testing.T) {
	pub, _ := authTestKeyPair(t)
	_, base, wsURL := authTestServer(t, "agent-a", pub, 5*time.Second)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, data := readFrame(t, conn, 2*time.Second)
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	bad := &AuthResponse{
		Envelope:  Envelope{Type: TypeAuthResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:   "agent-a",
		Nonce:     challenge.Nonce,
		Signature: "abcd", // hex, but not a 64-byte ed25519 signature
	}
	out, _ := Marshal(bad)
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write short signature: %v", err)
	}
	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed || !strings.Contains(errMsg.Error.Message, "malformed") {
		t.Fatalf("short-signature refusal = %s %q, want AUTH_FAILED naming a malformed signature", errMsg.Error.Code, errMsg.Error.Message)
	}

	// The server closes after a fatal refusal: the next read must NOT yield a
	// frame (a socket left open would let a client keep guessing).
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, data, err := conn.ReadMessage(); err == nil {
		t.Fatalf("the refused socket stayed usable — read %s after a malformed signature", data)
	}
	if ids := peerIDs(t, base); len(ids) != 0 {
		t.Errorf("a malformed-signature connection became a peer: /mesh/peers listed %v", ids)
	}
}

// TestMeshAuthFailedKindRejected: the server verifies the peer came for the
// right reason — a handshake that drops the peer is refused, and the refusal
// is on the wire, not a bare close.
func TestMeshAuthFailedKindRejected(t *testing.T) {
	pub, priv := authTestKeyPair(t)
	_, _, wsURL := authTestServer(t, "agent-a", pub, 2*time.Second)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, data := readFrame(t, conn, 2*time.Second)
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	// A peer that can sign, but answers with the REGISTER frame instead.
	reg := &Register{
		Envelope: Envelope{Type: TypeRegister, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:  "agent-a",
	}
	out, _ := Marshal(reg)
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write REGISTER: %v", err)
	}
	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed {
		t.Fatalf("refusal code = %q, want %q", errMsg.Error.Code, ErrCodeAuthFailed)
	}
	if !strings.Contains(errMsg.Error.Message, "REGISTER") {
		t.Errorf("refusal message = %q, want it to name the frame that was refused", errMsg.Error.Message)
	}

	// And it can still authenticate afterwards on the same socket.
	good := &AuthResponse{
		Envelope:  Envelope{Type: TypeAuthResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
		AgentID:   "agent-a",
		Nonce:     challenge.Nonce,
		Signature: hex.EncodeToString(ed25519.Sign(priv, MeshAuthPayload("agent-a", challenge.Nonce))),
	}
	out, _ = Marshal(good)
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write AUTH_RESPONSE: %v", err)
	}
	if env, data := readFrame(t, conn, 2*time.Second); env.Type != TypeAuthOK {
		t.Fatalf("frame after a correct answer = %q, want AUTH_OK (%s)", env.Type, data)
	}
}

// TestMeshAuthRequiredWithNoKeyProviderFailsClosed: a server configured to
// require authentication but wired to no key source refuses every connect. The
// alternative — admitting them — would make the flag a no-op.
func TestMeshAuthRequiredWithNoKeyProviderFailsClosed(t *testing.T) {
	cfg := DefaultMeshConfig("crier-under-test")
	cfg.Auth = MeshAuthConfig{Required: true, Timeout: time.Second}
	m := NewMesh(cfg) // deliberately no SetAgentKeyProvider
	router := mux.NewRouter()
	router.HandleFunc("/mesh/connect/{agentID}", HandleConnect(m)).Methods("GET")
	server := httptest.NewServer(router)
	defer server.Close()
	defer m.Stop()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh/connect/agent-a"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	errMsg := readErrorMessage(t, conn, 2*time.Second)
	if errMsg.Error.Code != ErrCodeAuthFailed {
		t.Fatalf("refusal code = %q, want %q", errMsg.Error.Code, ErrCodeAuthFailed)
	}
	if !strings.Contains(errMsg.Error.Message, "no agent key provider") {
		t.Errorf("refusal message = %q, want it to name the missing provider", errMsg.Error.Message)
	}
	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers = %d, want 0 — a required handshake with no key source must admit nobody", m.ActivePeers())
	}
}

// TestMeshConnectPeerCompletesHandshake exercises the CLIENT half end to end
// against the real accept path: Mesh.ConnectPeer answers the challenge with the
// configured signing key and is admitted.
func TestMeshConnectPeerCompletesHandshake(t *testing.T) {
	pub, priv := authTestKeyPair(t)
	m, base, _ := authTestServer(t, "agent-a", pub, 5*time.Second)

	client := NewMesh(MeshConfig{
		AgentID:           "agent-a",
		KeepaliveInterval: time.Hour,
		RequestTimeout:    time.Second,
		Auth:              MeshAuthConfig{Required: true, Timeout: 5 * time.Second, SigningKey: priv},
	})
	defer client.Stop()

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/agent-a"
	if err := client.ConnectPeer(context.Background(), "crier-under-test", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	if m.ActivePeers() != 1 {
		t.Fatalf("server ActivePeers = %d, want 1", m.ActivePeers())
	}
	waitForPeers(t, base, "agent-a", 2*time.Second)
}

// TestMeshConnectPeerSurfacesAuthRefusal: a client that cannot answer (no
// signing key) gets the server's refusal as a CONNECT ERROR, not a timeout —
// the difference between an operator reading "signature verification failed"
// and one waiting out a deadline with no information.
func TestMeshConnectPeerSurfacesAuthRefusal(t *testing.T) {
	pub, _ := authTestKeyPair(t)
	_, base, _ := authTestServer(t, "agent-a", pub, 5*time.Second)

	client := NewMesh(MeshConfig{
		AgentID:           "agent-a",
		KeepaliveInterval: time.Hour,
		RequestTimeout:    time.Second,
		Auth:              MeshAuthConfig{Required: true, Timeout: 5 * time.Second}, // no signing key
	})
	defer client.Stop()

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/mesh/connect/agent-a"
	err := client.ConnectPeer(context.Background(), "crier-under-test", wsURL)
	if err == nil {
		t.Fatal("ConnectPeer succeeded without a signing key against a server that requires authentication")
	}
	if !strings.Contains(err.Error(), "no signing key") {
		t.Errorf("ConnectPeer error = %v, want it to name the missing signing key", err)
	}
	if client.ActivePeers() != 0 {
		t.Errorf("client ActivePeers = %d, want 0", client.ActivePeers())
	}
}

// TestMeshAuthRequestSourceCannotBeSpoofed is the reason the handshake is worth
// anything: an admitted peer that names ANOTHER agent as a REQUEST's source is
// refused FORBIDDEN and the target never sees the frame. Without this, a
// verified socket could still speak as anyone one layer up.
func TestMeshAuthRequestSourceCannotBeSpoofed(t *testing.T) {
	// Auth is ON for the frame rules, which is the only thing under test here:
	// the peers below are admitted directly, as the accept path does after a
	// successful handshake.
	m := newAuthOnMesh(t)

	targetConn := acceptTestPeer(t, m, "target")
	attackerConn := acceptTestPeer(t, m, "attacker")

	req := &Request{
		Envelope: Envelope{Type: TypeRequest, Version: 1, MessageID: "spoof-1", Timestamp: time.Now()},
		Source:   PeerRef{AgentID: "victim"},
		Target:   PeerRef{AgentID: "target"},
		Method:   "POST",
		Path:     "/do-something-privileged",
		TraceID:  "spoof-1-trace",
	}
	out, err := Marshal(req)
	if err != nil {
		t.Fatalf("marshal REQUEST: %v", err)
	}
	if err := attackerConn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}

	// The attacker is told it was refused...
	_ = attackerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := attackerConn.ReadMessage()
	if err != nil {
		t.Fatalf("the refused requester got no ERROR frame: %v", err)
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(data, &errMsg); err != nil {
		t.Fatalf("refusal is not an ERROR frame: %v (%s)", err, data)
	}
	if errMsg.Error.Code != ErrCodeForbidden {
		t.Errorf("refusal code = %q, want %q (%s)", errMsg.Error.Code, ErrCodeForbidden, errMsg.Error.Message)
	}
	if errMsg.RequestID != "spoof-1" {
		t.Errorf("refusal request_id = %q, want the refused REQUEST's message_id so it correlates", errMsg.RequestID)
	}

	// ...and the target NEVER receives the impersonated request.
	expectNoFrame(t, targetConn, 300*time.Millisecond,
		"the target received a REQUEST whose source named another agent")
}

// TestMeshAuthRequestOwnSourceStillForwarded: the rule above must not break the
// normal case — an agent requesting as ITSELF is forwarded exactly as before.
func TestMeshAuthRequestOwnSourceStillForwarded(t *testing.T) {
	m := newAuthOnMesh(t)
	targetConn := acceptTestPeer(t, m, "target")
	requesterConn := acceptTestPeer(t, m, "requester")

	req := &Request{
		Envelope: Envelope{Type: TypeRequest, Version: 1, MessageID: "legit-1", Timestamp: time.Now()},
		Source:   PeerRef{AgentID: "requester"},
		Target:   PeerRef{AgentID: "target"},
		Method:   "GET",
		Path:     "/ping",
		TraceID:  "legit-1-trace",
	}
	out, _ := Marshal(req)
	if err := requesterConn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}

	_ = targetConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := targetConn.ReadMessage()
	if err != nil {
		t.Fatalf("the target received nothing: %v", err)
	}
	var got Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("forwarded frame is not an envelope: %v", err)
	}
	if got.Type != TypeRequest || got.MessageID != "legit-1" {
		t.Fatalf("target received %s %s, want REQUEST legit-1", got.Type, got.MessageID)
	}

	// And no refusal was sent to the requester.
	_ = requesterConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, data, err := requesterConn.ReadMessage(); err == nil {
		t.Fatalf("the requester received an unexpected frame: %s", data)
	}
}

// TestMeshAuthReplyMustComeFromTheTarget: a RESPONSE for a route whose target
// is another peer is not a reply at all, and is neither forwarded to the
// requester nor mistaken for one.
func TestMeshAuthReplyMustComeFromTheTarget(t *testing.T) {
	m := newAuthOnMesh(t)
	requesterConn := acceptTestPeer(t, m, "requester")
	acceptTestPeer(t, m, "target")
	intruderConn := acceptTestPeer(t, m, "intruder")

	req := &Request{
		Envelope: Envelope{Type: TypeRequest, Version: 1, MessageID: "pair-1", Timestamp: time.Now()},
		Source:   PeerRef{AgentID: "requester"},
		Target:   PeerRef{AgentID: "target"},
		Method:   "GET",
		Path:     "/ping",
		TraceID:  "pair-1-trace",
	}
	out, _ := Marshal(req)
	if err := requesterConn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}

	// Let the route be recorded (the frame reaches the target).
	time.Sleep(50 * time.Millisecond)

	resp := &Response{
		Envelope:   Envelope{Type: TypeResponse, Version: 1, MessageID: "pair-1-resp", Timestamp: time.Now()},
		RequestID:  "pair-1",
		Source:     PeerRef{AgentID: "intruder"},
		StatusCode: 200,
		TraceID:    "pair-1-trace",
	}
	resp.Body, _ = json.Marshal(map[string]any{"injected": true})
	out, _ = Marshal(resp)
	if err := intruderConn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write RESPONSE: %v", err)
	}

	// The intruder is told the reply was refused...
	_ = intruderConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := intruderConn.ReadMessage()
	if err != nil {
		t.Fatalf("the refused responder got no ERROR frame: %v", err)
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(data, &errMsg); err != nil {
		t.Fatalf("refusal is not an ERROR frame: %v", err)
	}
	if errMsg.Error.Code != ErrCodeForbidden {
		t.Errorf("refusal code = %q, want %q (%s)", errMsg.Error.Code, ErrCodeForbidden, errMsg.Error.Message)
	}

	// ...and the requester never sees the injected response.
	_ = requesterConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, data, err := requesterConn.ReadMessage(); err == nil {
		t.Fatalf("the requester received a reply from a peer it never addressed: %s", data)
	}
}

// TestMeshAuthFramesOutsideHandshakeAreRefused: the AUTH_* types are context
// bound. On an admitted socket they are a client error, refused with
// INVALID_MESSAGE rather than silently ignored.
func TestMeshAuthFramesOutsideHandshakeAreRefused(t *testing.T) {
	m := newAuthOnMesh(t)
	conn := acceptTestPeer(t, m, "target")

	ok := &AuthOK{
		Envelope: Envelope{Type: TypeAuthOK, Version: 1, MessageID: "late-auth", Timestamp: time.Now()},
		AgentID:  "target",
	}
	out, _ := Marshal(ok)
	if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write AUTH_OK: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no refusal for an out-of-handshake AUTH_OK: %v", err)
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(data, &errMsg); err != nil {
		t.Fatalf("refusal is not an ERROR frame: %v (%s)", err, data)
	}
	if errMsg.Error.Code != ErrCodeInvalidMessage {
		t.Errorf("refusal code = %q, want %q", errMsg.Error.Code, ErrCodeInvalidMessage)
	}
	if errMsg.RequestID != "late-auth" {
		t.Errorf("refusal request_id = %q, want the offending frame's message_id", errMsg.RequestID)
	}
}

// TestMeshAuthOffKeepsLegacyBehaviour is the migration guarantee: with the flag
// unset (the shipped default) nothing about the accept path or the frame rules
// changes. A socket that never authenticates still becomes a peer, and a
// REQUEST whose source names another agent is still forwarded — the exact
// behaviour this row's fix must not silently break.
func TestMeshAuthOffKeepsLegacyBehaviour(t *testing.T) {
	cfg := DefaultMeshConfig("crier-under-test")
	cfg.KeepaliveInterval = time.Hour
	m := NewMesh(cfg) // Auth zero value = off
	if m.AuthRequired() {
		t.Fatal("DefaultMeshConfig must not require mesh authentication — the flag is opt-in")
	}

	router := mux.NewRouter()
	router.HandleFunc("/mesh/connect/{agentID}", HandleConnect(m)).Methods("GET")
	router.HandleFunc("/mesh/peers", HandlePeers(m)).Methods("GET")
	server := httptest.NewServer(router)
	defer server.Close()
	defer m.Stop()

	// 1. A raw socket with no handshake becomes a peer immediately.
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh/connect/agent-a"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	waitForPeers(t, server.URL, "agent-a", 2*time.Second)

	// 2. No AUTH_CHALLENGE is ever sent: the first frame a default server sends
	// is a KEEPALIVE (30s away), so a short read must time out rather than
	// produce an auth frame.
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	if _, data, err := conn.ReadMessage(); err == nil {
		t.Fatalf("a default (auth-off) server sent %s to a fresh connection — the flag is supposed to be the only thing that puts an AUTH_* frame on this wire", data)
	}

	// 3. The legacy frame rule holds: a source that names another agent is
	// forwarded untouched while authentication is off.
	targetConn := acceptTestPeer(t, m, "target")
	legacyConn := acceptTestPeer(t, m, "legacy")
	req := &Request{
		Envelope: Envelope{Type: TypeRequest, Version: 1, MessageID: "legacy-1", Timestamp: time.Now()},
		Source:   PeerRef{AgentID: "someone-else"},
		Target:   PeerRef{AgentID: "target"},
		Method:   "GET",
		Path:     "/ping",
		TraceID:  "legacy-1-trace",
	}
	out, _ := Marshal(req)
	if err := legacyConn.WriteMessage(websocket.TextMessage, out); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}
	_ = targetConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, data, err := targetConn.ReadMessage(); err != nil {
		t.Fatalf("auth-off mesh no longer forwards a REQUEST: %v", err)
	} else if !strings.Contains(string(data), "legacy-1") {
		t.Fatalf("forwarded frame = %s, want the legac-1 REQUEST", data)
	}
}

// ---------------------------------------------------------------------------
// helpers for the frame-rule tests
// ---------------------------------------------------------------------------

// newAuthOnMesh returns a mesh with authentication REQUIRED (so the frame rules
// are live) and no timers that tick during a test.
func newAuthOnMesh(t *testing.T) *Mesh {
	t.Helper()
	cfg := DefaultMeshConfig("crier-under-test")
	cfg.KeepaliveInterval = time.Hour
	cfg.Auth = MeshAuthConfig{Required: true, Timeout: 2 * time.Second}
	m := NewMesh(cfg)
	t.Cleanup(m.Stop)
	return m
}

// acceptTestPeer wires a real client WebSocket to the mesh as an already
// admitted peer and returns the client end — the state the accept path reaches
// only after a successful handshake (and, with auth off, immediately).
func acceptTestPeer(t *testing.T, m *Mesh, peerID string) *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{}
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		select {
		case accepted <- conn:
		default:
			conn.Close()
		}
	}))
	t.Cleanup(server.Close)

	clientConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial test peer socket: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })

	var serverConn *websocket.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out wiring the %s peer socket", peerID)
	}
	pc := NewAcceptedPeerConnection(peerID, serverConn)
	pc.StartReadLoop()
	m.AcceptPeer(peerID, pc)
	return clientConn
}
