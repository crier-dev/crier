package mesh

// auth.go — DF-CRIER-287: the mesh connect handshake.
//
// Before this file, `GET /mesh/connect/{agentID}` proved NOTHING about the
// caller: the agent id in the path was a claim, and the mesh — the one lane
// crier advertises for agent-to-agent request/response — was therefore the one
// lane where any network-reachable client could speak as any registered agent
// (the registry's inbox lane has enforced a per-agent ed25519 signature for
// longer than the mesh has existed). The external review that filed this row
// measured it: `grep -rnE 'requireAgent|Signature|token|auth' internal/mesh/*.go`
// returned zero hits at HEAD.
//
// The fix is a challenge at connect, not a per-frame signature:
//
//  1. the socket is upgraded, but NOT yet a peer;
//  2. the server sends AUTH_CHALLENGE naming the path identity and a
//     single-use nonce;
//  3. the client answers AUTH_RESPONSE with the hex ed25519 signature over
//     MeshAuthPayload(agent_id, nonce), made with the private key whose public
//     half the REGISTRY holds for that agent;
//  4. only after the signature verifies does the server send AUTH_OK and admit
//     the connection to the peer table (start its read loop, register it,
//     begin its keepalive). A peer that never answers is refused with
//     ERROR/AUTH_FAILED and never appears in `GET /mesh/peers`.
//
// Once admitted, the socket's identity is a verified fact rather than a claim,
// so the frame-level rules in peer.go can bind to it: a REQUEST whose
// `source.agent_id` is not the authenticated peer is refused FORBIDDEN, and a
// reply may only be sent by the peer the request was addressed to. Both rules
// hold only while mesh authentication is on (the flag is off by default — see
// docs/mesh-protocol.md §Authentication for the migration note).
//
// Everything here is opt-in: with `CR_REQUIRE_MESH_AUTH` unset the accept path
// is byte-for-byte what it was (no challenge, no new frame on the wire, no
// registry lookup), so an existing single-host deployment is unaffected.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// meshAuthPayloadPrefix domain-separates the mesh handshake payload from every
// other ed25519 signature crier asks an agent to make: the registry's inbox
// signature covers "<METHOD>\n<path>\n<unix-seconds>", which shares neither
// this prefix nor this line shape, so a signature made for one lane can never
// be replayed on the other.
const meshAuthPayloadPrefix = "mesh-auth-v1"

// MeshAuthPayload returns the exact bytes an AUTH_RESPONSE signature covers:
//
//	mesh-auth-v1\n<agent_id>\n<nonce>
//
// It is exported because it is a cross-implementation contract — a client that
// reimplements the mesh (the Python worked example in docs/mesh-protocol.md,
// or any other language) has to sign these exact bytes. The payload binds the
// CLAIMED identity to THIS challenge, so a signature captured from an earlier
// connection verifies against neither a different agent id nor a different
// nonce. Signing is raw ed25519 over these bytes (no pre-hash, no PKCS#1
// framing): `openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt`.
func MeshAuthPayload(agentID, nonce string) []byte {
	return []byte(meshAuthPayloadPrefix + "\n" + agentID + "\n" + nonce)
}

// meshAuthNonceBytes is the challenge nonce length before hex encoding (32 hex
// characters on the wire). It is a single-use value minted per connection from
// crypto/rand; 128 bits is far past guessable for a value that also expires.
const meshAuthNonceBytes = 16

// DefaultMeshAuthTimeout bounds the connect handshake when the caller does not
// set one: long enough for a slow network round trip, short enough that a
// silent client cannot hold a pre-auth socket open indefinitely.
func DefaultMeshAuthTimeout() time.Duration { return 10 * time.Second }

// AgentKeyProvider resolves the ed25519 public key the registry holds for an
// agent. The mesh does not depend on the registry package — the server passes
// an adapter over whatever registry.Store is in force, so the SAME identity
// record that gates the inbox lane gates the mesh lane.
//
// A returned error (unknown agent, or a lookup failure) fails the handshake
// closed: the connect is answered AUTH_FAILED, never admitted.
type AgentKeyProvider interface {
	RegisteredPublicKey(agentID string) (ed25519.PublicKey, error)
}

// MeshAuthConfig is the mesh half of the authentication configuration.
type MeshAuthConfig struct {
	// Required makes an incoming connect prove possession of the agent's
	// registered ed25519 private key before it is admitted as a peer
	// (CR_REQUIRE_MESH_AUTH). Default FALSE: with it unset the accept path is
	// unchanged, which is what keeps an existing deployment working.
	Required bool
	// Timeout bounds the challenge/response exchange (CR_MESH_AUTH_TIMEOUT_S,
	// default DefaultMeshAuthTimeout). Non-positive means "use the default".
	Timeout time.Duration
	// SigningKey is the OUTBOUND identity: the ed25519 private key this mesh
	// answers a server's AUTH_CHALLENGE with (Mesh.ConnectPeer). Nil means
	// this side cannot authenticate itself, so a server that requires auth
	// refuses it with AUTH_FAILED.
	SigningKey ed25519.PrivateKey
}

// effectiveTimeout normalizes a zero/negative Timeout to the default, so every
// caller bounds the handshake with a real deadline.
func (c MeshAuthConfig) effectiveTimeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultMeshAuthTimeout()
	}
	return c.Timeout
}

// newAuthNonce mints a single-use hex nonce.
func newAuthNonce() (string, error) {
	b := make([]byte, meshAuthNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mesh auth: read nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// authenticateIncoming runs the SERVER half of the handshake on a socket that
// has been upgraded but not yet admitted as a peer.
//
// It returns true only when the peer proved possession of agentID's registered
// private key; in that case the caller admits the connection. Every other path
// returns false having already written the refusal on the wire and closed the
// socket, so a failed handshake is visible to the client and leaves no peer
// behind.
func (m *Mesh) authenticateIncoming(conn *websocket.Conn, agentID string, auth MeshAuthConfig) bool {
	timeout := auth.effectiveTimeout()

	keys := m.keyProvider()
	if keys == nil {
		// A server configured to require mesh authentication without a key
		// source cannot verify anything, so it refuses everything rather than
		// admitting unverified peers on a misconfiguration.
		return refuseConnect(conn, ErrCodeAuthFailed,
			"mesh authentication is required but this server has no agent key provider configured")
	}

	pub, err := keys.RegisteredPublicKey(agentID)
	if err != nil {
		// Fail closed, and name the real reason: the most common cause is an
		// agent that was never registered (or registered without a key), and
		// that is fixable by the operator — a request to a different endpoint.
		return refuseConnect(conn, ErrCodeAuthFailed,
			fmt.Sprintf("mesh authentication failed for %q: no registered ed25519 public key (%v) — register the agent with its public key (POST /agents) first", agentID, err))
	}
	if len(pub) != ed25519.PublicKeySize {
		return refuseConnect(conn, ErrCodeAuthFailed,
			fmt.Sprintf("mesh authentication failed for %q: the registered key is not a %d-byte ed25519 public key, so no signature can verify against it", agentID, ed25519.PublicKeySize))
	}

	nonce, err := newAuthNonce()
	if err != nil {
		slog.Error("mesh: challenge nonce", "agent_id", agentID, "error", err)
		return refuseConnect(conn, ErrCodeInternal, "mesh authentication unavailable: could not mint a challenge nonce")
	}
	expires := time.Now().Add(timeout)
	challenge := &AuthChallenge{
		Envelope: Envelope{
			Type:      TypeAuthChallenge,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		AgentID:   agentID,
		Nonce:     nonce,
		ExpiresAt: expires,
	}
	data, err := Marshal(challenge)
	if err != nil {
		slog.Error("mesh: marshal challenge", "agent_id", agentID, "error", err)
		return false
	}
	if err := writeText(conn, data); err != nil {
		slog.Debug("mesh: challenge undeliverable", "agent_id", agentID, "error", err)
		return false
	}

	// The read deadline is the second half of the challenge expiry: a peer that
	// receives the challenge and then says nothing is refused by the deadline
	// rather than held open. It is cleared on success, before the admitted
	// connection takes over reading.
	if err := conn.SetReadDeadline(expires); err != nil {
		slog.Warn("mesh: set auth read deadline", "agent_id", agentID, "error", err)
		return refuseConnect(conn, ErrCodeInternal, "mesh authentication unavailable: could not bound the challenge deadline")
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// A deadline that expires is a handshake that did not happen, and
			// the client is told so on the wire before the socket closes: a
			// silent client sees WHY it is not a peer rather than a bare
			// disconnect it cannot distinguish from a crash.
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return refuseConnect(conn, ErrCodeAuthFailed,
					fmt.Sprintf("no AUTH_RESPONSE within %s: the challenge expired before this connection proved it holds %q's registered key", timeout, agentID))
			}
			slog.Warn("mesh: authentication refused (no valid AUTH_RESPONSE)",
				"agent_id", agentID, "error", err)
			_ = conn.Close()
			return false
		}

		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			// Readable enough to be refused with the protocol's malformed-frame
			// code, but nothing to correlate to: no request_id.
			refuseFrame(conn, "", ErrCodeInvalidMessage,
				fmt.Sprintf("malformed frame: not a JSON envelope (%v)", err))
			continue
		}
		if env.Type != TypeAuthResponse {
			// Only the answer is permitted before admission. Name the frame
			// that arrived and keep the socket alive for one more attempt
			// (within the same deadline) — a client that sent its REGISTER
			// first is told exactly why it did not count.
			refuseFrame(conn, env.MessageID, ErrCodeAuthFailed,
				fmt.Sprintf("frame %q refused: the first frame on a connection that requires mesh authentication must be AUTH_RESPONSE (this socket has not authenticated %q)", env.Type, agentID))
			continue
		}

		var resp AuthResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			refuseFrame(conn, env.MessageID, ErrCodeInvalidMessage,
				fmt.Sprintf("malformed AUTH_RESPONSE frame: %v", err))
			continue
		}
		if resp.AgentID != agentID {
			refuseConnect(conn, ErrCodeAuthFailed,
				fmt.Sprintf("AUTH_RESPONSE names agent_id %q but this connection claimed %q — an agent may only authenticate as itself", resp.AgentID, agentID))
			return false
		}
		if subtle.ConstantTimeCompare([]byte(resp.Nonce), []byte(nonce)) != 1 {
			refuseConnect(conn, ErrCodeAuthFailed,
				"AUTH_RESPONSE nonce does not match the challenge this connection was issued")
			return false
		}
		sig, err := hex.DecodeString(resp.Signature)
		if err != nil || len(sig) != ed25519.SignatureSize {
			refuseConnect(conn, ErrCodeAuthFailed,
				fmt.Sprintf("AUTH_RESPONSE signature is malformed: expected the hex encoding of a %d-byte ed25519 signature, got %d character(s)", ed25519.SignatureSize, len(resp.Signature)))
			return false
		}
		if !ed25519.Verify(pub, MeshAuthPayload(agentID, nonce), sig) {
			refuseConnect(conn, ErrCodeAuthFailed,
				fmt.Sprintf("signature verification failed: the AUTH_RESPONSE signature does not verify against the public key registered for %q", agentID))
			return false
		}

		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			slog.Warn("mesh: clear auth read deadline", "agent_id", agentID, "error", err)
			return refuseConnect(conn, ErrCodeInternal, "mesh authentication unavailable: could not clear the challenge deadline")
		}
		ok := &AuthOK{
			Envelope: Envelope{
				Type:      TypeAuthOK,
				Version:   1,
				MessageID: newMessageID(),
				Timestamp: time.Now(),
			},
			AgentID: agentID,
		}
		okData, err := Marshal(ok)
		if err != nil || writeText(conn, okData) != nil {
			slog.Warn("mesh: AUTH_OK undeliverable", "agent_id", agentID, "error", err)
			_ = conn.Close()
			return false
		}
		slog.Info("mesh: peer authenticated", "agent_id", agentID, "auth", "ed25519-challenge")
		return true
	}
}

// answerAuthChallenge runs the CLIENT half of the handshake: it verifies the
// challenge is addressed to this mesh's own identity and answers it with the
// configured signing key.
//
// An error means the challenge cannot be answered (no signing key, a challenge
// for another identity, an unreadable frame) — the caller reports it rather
// than sending a signature that could not verify.
func (m *Mesh) answerAuthChallenge(conn *PeerConnection, peerID string, data []byte, signingKey ed25519.PrivateKey) error {
	var challenge AuthChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		return fmt.Errorf("mesh auth: malformed AUTH_CHALLENGE from %s: %w", peerID, err)
	}
	if challenge.AgentID != m.agentID {
		return fmt.Errorf("mesh auth: %s challenged identity %q, but this mesh is %q — it cannot sign as another agent",
			peerID, challenge.AgentID, m.agentID)
	}
	if len(signingKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("mesh auth: %s requires authentication but no signing key is configured (set MeshAuthConfig.SigningKey to the ed25519 key whose public half is registered for %q)",
			peerID, m.agentID)
	}
	if challenge.Nonce == "" {
		return fmt.Errorf("mesh auth: %s sent an AUTH_CHALLENGE with no nonce", peerID)
	}

	resp := &AuthResponse{
		Envelope: Envelope{
			Type:      TypeAuthResponse,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		AgentID:   m.agentID,
		Nonce:     challenge.Nonce,
		Signature: hex.EncodeToString(ed25519.Sign(signingKey, MeshAuthPayload(m.agentID, challenge.Nonce))),
	}
	out, err := Marshal(resp)
	if err != nil {
		return fmt.Errorf("mesh auth: marshal AUTH_RESPONSE: %w", err)
	}
	if err := conn.Send(out); err != nil {
		return fmt.Errorf("mesh auth: send AUTH_RESPONSE to %s: %w", peerID, err)
	}
	return nil
}

// writeText writes one text frame, bounded like every other mesh send so a
// peer that stops reading cannot wedge the writer.
func writeText(conn *websocket.Conn, data []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

// refuseFrame writes an ERROR frame on a connection that is not (yet) a peer.
// It is the pre-admission twin of Mesh.sendErrorTo, which needs a registered
// connection and therefore cannot serve the handshake.
func refuseFrame(conn *websocket.Conn, requestID, code, message string) {
	errMsg := &ErrorMessage{
		Envelope: Envelope{
			Type:      TypeError,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		RequestID: requestID,
		Error:     ErrorDetail{Code: code, Message: message},
	}
	data, err := Marshal(errMsg)
	if err != nil {
		return
	}
	_ = writeText(conn, data)
}

// refuseConnect writes the refusal and closes the socket: the handshake is
// over, so unlike refuseFrame there is nothing left to answer on.
func refuseConnect(conn *websocket.Conn, code, message string) bool {
	slog.Warn("mesh: authentication refused", "code", code, "reason", message)
	refuseFrame(conn, "", code, message)
	_ = conn.Close()
	return false
}
