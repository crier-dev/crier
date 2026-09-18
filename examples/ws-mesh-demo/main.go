// Command ws-mesh-demo is a runnable, no-install demo client for the crier
// relay + mesh endpoints. It has three subcommands:
//
//	subscribe -topic TOPIC [-once]        listen on ws://…/relay/subscribe/<topic>
//	peer      -agent AGENT_ID [-respond]  join the P2P mesh as <agentID>,
//	                                      optionally answering inbound REQUESTs
//	roundtrip -agent A -target B          connect as A, send one REQUEST to B,
//	                                      correlate the RESPONSE by `request_id`
//
// It uses only the Go standard library, github.com/gorilla/websocket (v1.5.3,
// already in go.mod) and this module's own internal/mesh wire types — zero
// external installs. Every frame it puts on the wire is built by the same
// structs and mesh.Marshal the server uses, so the demo cannot drift from
// docs/mesh-protocol.md.
//
// Addresses CR-GAP-050 and DF-CRIER-3 (the REQUEST → RESPONSE leg, which the
// README's mesh section documents but does not show how to run).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/crier-dev/crier/internal/mesh"
)

// defaultBaseURL is the demo's scratch default (run-demo.sh starts its relay
// here). It deliberately avoids the fleet's long-lived ports (:8767 relay,
// :18767 docker-published crier) so a bare `peer -agent X` cannot connect to
// some other server by accident. run-demo.sh always passes -url explicitly.
const defaultBaseURL = "http://127.0.0.1:18961"

func usage() {
	fmt.Fprintf(os.Stderr, `ws-mesh-demo — crier relay + mesh demo client (CR-GAP-050, DF-CRIER-3)

Usage:
  ws-mesh-demo [-url BASE] subscribe -topic TOPIC [-once]
  ws-mesh-demo [-url BASE] peer -agent AGENT_ID [-respond]
  ws-mesh-demo [-url BASE] roundtrip -agent AGENT -target AGENT

Commands:
  subscribe  connect to ws://…/relay/subscribe/<topic> and print events.
             Prints "SUBSCRIBED <topic>" after the upgrade, then one
             "EVENT <payload>" line per received event (payload is the raw
             event JSON the relay fans out). With -once, exits 0 after the
             first event.
  peer       connect to ws://…/mesh/connect/<agentID>, send the one-way
             REGISTER frame and hold the connection open so the server keeps
             the peer registered. Prints "PEER CONNECTED <agentID>" after the
             upgrade. With -respond, inbound REQUEST frames are answered with
             a RESPONSE (see -status/-body/-respond-delay), and every other
             frame type (KEEPALIVE included) is ignored and reported.
  roundtrip  connect as -agent, send one REQUEST to -target and wait for the
             RESPONSE that echoes the REQUEST's message_id as request_id.
             KEEPALIVE frames arriving on the same socket are ignored. Prints
             "ROUNDTRIP OK request_id=… status_code=… response_message_id=…
             keepalives_ignored=…" and exits 0 only when the reply correlated
             and carried the expected status code.

Flags:
  -url   server base URL (default %s)
`, defaultBaseURL)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("ws-mesh-demo: ")

	urlFlag := flag.String("url", defaultBaseURL, "server base URL (http:// or https://)")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}

	// Derive the WebSocket base from the HTTP base.
	wsBase := strings.Replace(*urlFlag, "http://", "ws://", 1)
	wsBase = strings.Replace(wsBase, "https://", "wss://", 1)

	switch flag.Arg(0) {
	case "subscribe":
		os.Exit(cmdSubscribe(wsBase, flag.Args()[1:]))
	case "peer":
		os.Exit(cmdPeer(wsBase, flag.Args()[1:]))
	case "roundtrip":
		os.Exit(cmdRoundtrip(wsBase, flag.Args()[1:]))
	default:
		fmt.Fprintf(os.Stderr, "ws-mesh-demo: unknown subcommand %q\n", flag.Arg(0))
		usage()
		os.Exit(2)
	}
}

// newMessageID mirrors the server's 24-hex message_id (internal/mesh/message.go
// newMessageID): 12 random bytes, hex-encoded.
func newMessageID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is fatal for a protocol that keys correlation on
		// these ids.
		log.Fatalf("crypto/rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// dialMesh opens ws://<base>/mesh/connect/<agentID>. The path segment is the
// client's identity — the mesh has no other authentication.
func dialMesh(wsBase, agentID string) (*websocket.Conn, error) {
	u := fmt.Sprintf("%s/mesh/connect/%s", wsBase, url.PathEscape(agentID))
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", u, err)
	}
	return conn, nil
}

// sendRegister sends the one-way REGISTER frame every mesh client sends on
// connect. The server never replies (there is no REGISTER_ACK on the wire) and
// the handshake payload is informational — but it is part of the contract in
// docs/mesh-protocol.md §REGISTER, so the demo sends it.
func sendRegister(conn *websocket.Conn, agentID string) error {
	reg := mesh.Register{
		Envelope: mesh.Envelope{
			Type:      mesh.TypeRegister,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		AgentID:    agentID,
		LeaseID:    "",
		LeaseTTLMs: 3600000,
		Capabilities: mesh.Capabilities{
			Version:               "0.1.0",
			Topics:                []string{},
			MaxConcurrentSessions: 10,
		},
	}
	data, err := mesh.Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshal REGISTER: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("send REGISTER: %w", err)
	}
	fmt.Printf("REGISTER SENT message_id=%s agent_id=%s\n", reg.MessageID, agentID)
	return nil
}

// lightEnvelope is the minimum every frame carries, used to route a received
// frame by `type` before decoding it into its full struct.
type lightEnvelope struct {
	Type      mesh.MessageType `json:"type"`
	MessageID string           `json:"message_id"`
	RequestID string           `json:"request_id"`
}

// frameKind is the verdict of the client-side filter.
type frameKind int

const (
	// frameIgnore is any frame that is not the reply we are waiting for:
	// KEEPALIVE (the server sends one every KeepaliveInterval —
	// internal/mesh/peer.go keepaliveLoop, 30s by default), an unrelated
	// REGISTER/REGISTER_ACK, or a RESPONSE/ERROR addressed to a different
	// request_id (someone else's reply on a shared socket).
	frameIgnore frameKind = iota
	// frameReply is a RESPONSE/ERROR whose request_id is the message_id of the
	// REQUEST we are awaiting.
	frameReply
)

// classifyInbound is the filter every mesh client must implement: the socket
// carries KEEPALIVE frames interleaved with the reply, so read in a loop and
// switch on `type` — never treat the next frame as the answer. `awaiting` is
// the message_id of the REQUEST being waited on; a reply correlates on
// request_id, never on the frame's own message_id.
func classifyInbound(typeName mesh.MessageType, requestID, awaiting string) frameKind {
	switch typeName {
	case mesh.TypeResponse, mesh.TypeError:
		if requestID == awaiting {
			return frameReply
		}
		return frameIgnore
	default:
		return frameIgnore
	}
}

// cmdSubscribe implements the "subscribe" subcommand. It exits 0 after the
// first received event when -once is set, 1 on any connection error.
func cmdSubscribe(wsBase string, args []string) int {
	fs := flag.NewFlagSet("subscribe", flag.ExitOnError)
	topic := fs.String("topic", "", "topic to subscribe to (required)")
	once := fs.Bool("once", false, "exit 0 after the first received event")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ws-mesh-demo subscribe -topic TOPIC [-once]\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *topic == "" {
		fmt.Fprintln(os.Stderr, "ws-mesh-demo subscribe: -topic is required")
		fs.Usage()
		return 2
	}

	u := fmt.Sprintf("%s/relay/subscribe/%s", wsBase, url.PathEscape(*topic))
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		log.Printf("subscribe: dial %s: %v", u, err)
		return 1
	}
	defer conn.Close()

	fmt.Printf("SUBSCRIBED %s\n", *topic)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			log.Printf("subscribe: read: %v", err)
			return 1
		}
		fmt.Printf("EVENT %s\n", payload)
		if *once {
			return 0
		}
	}
}

// cmdPeer implements the "peer" subcommand. It holds the WebSocket open
// (every frame is read, so gorilla's default ping handler keeps auto-ponging
// and the relay's keepalive never drops the peer) and exits 0 on clean close.
// With -respond it answers inbound REQUEST frames with a RESPONSE whose
// request_id is the REQUEST's message_id — the correlation contract in
// docs/mesh-protocol.md.
func cmdPeer(wsBase string, args []string) int {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	agent := fs.String("agent", "", "agent ID to register as (required)")
	respond := fs.Bool("respond", false, "answer inbound REQUEST frames with a RESPONSE")
	status := fs.Int("status", 200, "status_code to answer REQUESTs with (-respond)")
	body := fs.String("body", "", "raw JSON body for answered REQUESTs, e.g. '{\"pong\":true}' (-respond; empty omits the field)")
	respondDelay := fs.Duration("respond-delay", 0, "wait this long before answering a REQUEST (-respond; models a slow peer, and lets a caller see KEEPALIVE frames arrive mid-await)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ws-mesh-demo peer -agent AGENT_ID [-respond -status CODE -body JSON -respond-delay D]\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *agent == "" {
		fmt.Fprintln(os.Stderr, "ws-mesh-demo peer: -agent is required")
		fs.Usage()
		return 2
	}
	if *body != "" && !json.Valid([]byte(*body)) {
		fmt.Fprintf(os.Stderr, "ws-mesh-demo peer: -body must be valid JSON: %q\n", *body)
		fs.Usage()
		return 2
	}

	conn, err := dialMesh(wsBase, *agent)
	if err != nil {
		log.Printf("peer: %v", err)
		return 1
	}
	defer conn.Close()

	fmt.Printf("PEER CONNECTED %s\n", *agent)
	if err := sendRegister(conn, *agent); err != nil {
		log.Printf("peer: %v", err)
		return 1
	}

	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			// Connection closed: the relay removed us from the mesh.
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return 0
			}
			log.Printf("peer: read: %v", err)
			return 1
		}
		var env lightEnvelope
		if err := json.Unmarshal(frame, &env); err != nil {
			fmt.Printf("PEER FRAME IGNORED unparseable (%d bytes)\n", len(frame))
			continue
		}

		switch env.Type {
		case mesh.TypeRequest:
			var req mesh.Request
			if err := json.Unmarshal(frame, &req); err != nil {
				fmt.Printf("PEER REQUEST UNPARSEABLE (%d bytes)\n", len(frame))
				continue
			}
			fmt.Printf("REQUEST RECEIVED message_id=%s source=%s method=%s path=%s\n",
				req.MessageID, req.Source.AgentID, req.Method, req.Path)
			if !*respond {
				fmt.Printf("REQUEST UNANSWERED message_id=%s (peer started without -respond)\n", req.MessageID)
				continue
			}
			if *respondDelay > 0 {
				time.Sleep(*respondDelay)
			}
			resp := mesh.Response{
				Envelope: mesh.Envelope{
					Type:      mesh.TypeResponse,
					Version:   1,
					MessageID: newMessageID(),
					Timestamp: time.Now(),
				},
				// The correlation contract: request_id is the REQUEST's
				// message_id. A reply that does not echo it is dropped by the
				// server's route table and the requester hangs until timeout.
				RequestID:  req.MessageID,
				Source:     mesh.PeerRef{AgentID: *agent},
				StatusCode: *status,
				TraceID:    req.TraceID,
			}
			if *body != "" {
				resp.Body = json.RawMessage(*body)
			}
			data, err := mesh.Marshal(resp)
			if err != nil {
				fmt.Printf("RESPONSE MARSHAL FAILED request_id=%s: %v\n", req.MessageID, err)
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("peer: send RESPONSE: %v", err)
				return 1
			}
			fmt.Printf("RESPONSE SENT request_id=%s status=%d response_message_id=%s\n",
				resp.RequestID, resp.StatusCode, resp.MessageID)
		case mesh.TypeKeepalive:
			fmt.Printf("PEER KEEPALIVE IGNORED message_id=%s\n", env.MessageID)
		default:
			fmt.Printf("PEER FRAME IGNORED type=%s message_id=%s\n", env.Type, env.MessageID)
		}
	}
}

// cmdRoundtrip implements the "roundtrip" subcommand: the REQUEST → RESPONSE
// leg of docs/mesh-protocol.md, driven with nothing installed.
//
// Exit status is the whole assertion the shell driver needs: 0 only when a
// RESPONSE correlated with the REQUEST's message_id and carried
// -expect-status, and — when -keepalive-wait is set — only after at least one
// live KEEPALIVE frame arrived on the same socket and was ignored.
func cmdRoundtrip(wsBase string, args []string) int {
	fs := flag.NewFlagSet("roundtrip", flag.ExitOnError)
	agent := fs.String("agent", "", "agent ID to connect as (required)")
	target := fs.String("target", "", "agent ID the REQUEST is addressed to (required)")
	method := fs.String("method", "GET", "REQUEST method")
	path := fs.String("path", "/ping", "REQUEST path (the target's own application route)")
	body := fs.String("body", "", "raw JSON body for the REQUEST, e.g. '{\"hello\":\"world\"}' (empty omits the field)")
	expectStatus := fs.Int("expect-status", 200, "status_code the RESPONSE must carry")
	timeout := fs.Duration("timeout", 15*time.Second, "how long to wait for the RESPONSE")
	keepaliveWait := fs.Duration("keepalive-wait", 0,
		"after the RESPONSE, hold the socket up to this long for at least one live KEEPALIVE frame (0 = return as soon as the RESPONSE is correlated; KEEPALIVE frames are ignored either way)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ws-mesh-demo roundtrip -agent AGENT -target AGENT [-method M -path P -body JSON -expect-status N -timeout D -keepalive-wait D]\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *agent == "" || *target == "" {
		fmt.Fprintln(os.Stderr, "ws-mesh-demo roundtrip: -agent and -target are required")
		fs.Usage()
		return 2
	}
	if *body != "" && !json.Valid([]byte(*body)) {
		fmt.Fprintf(os.Stderr, "ws-mesh-demo roundtrip: -body must be valid JSON: %q\n", *body)
		fs.Usage()
		return 2
	}

	conn, err := dialMesh(wsBase, *agent)
	if err != nil {
		log.Printf("roundtrip: %v", err)
		return 1
	}
	defer conn.Close()

	if err := sendRegister(conn, *agent); err != nil {
		log.Printf("roundtrip: %v", err)
		return 1
	}
	fmt.Printf("ROUNDTRIP CONNECTED %s -> %s (%s)\n", *agent, *target, wsBase)

	req := mesh.Request{
		Envelope: mesh.Envelope{
			Type:      mesh.TypeRequest,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		Source:    mesh.PeerRef{AgentID: *agent},
		Target:    mesh.PeerRef{AgentID: *target},
		Method:    *method,
		Path:      *path,
		TraceID:   newMessageID(),
		TimeoutMs: int(timeout.Milliseconds()),
	}
	if *body != "" {
		var decoded any
		if err := json.Unmarshal([]byte(*body), &decoded); err != nil {
			fmt.Fprintf(os.Stderr, "ws-mesh-demo roundtrip: -body must be valid JSON: %v\n", err)
			return 2
		}
		req.Body = decoded
	}
	data, err := mesh.Marshal(req)
	if err != nil {
		log.Printf("roundtrip: marshal REQUEST: %v", err)
		return 1
	}
	fmt.Printf("REQUEST SENT message_id=%s target=%s method=%s path=%s trace_id=%s\n",
		req.MessageID, *target, *method, *path, req.TraceID)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("roundtrip: send REQUEST: %v", err)
		return 1
	}

	// Await the reply. Loop-recv and switch on `type`: KEEPALIVE frames (and
	// any other frame that is not our reply) must be ignored, and the reply is
	// the one whose request_id is this REQUEST's message_id — not simply the
	// next frame that arrives.
	keepalives := 0
	if err := conn.SetReadDeadline(time.Now().Add(*timeout)); err != nil {
		log.Printf("roundtrip: set read deadline: %v", err)
		return 1
	}
	var resp mesh.Response
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				fmt.Printf("ROUNDTRIP FAIL no RESPONSE for message_id=%s within %s (keepalives_ignored=%d)\n",
					req.MessageID, *timeout, keepalives)
				return 1
			}
			fmt.Printf("ROUNDTRIP FAIL read: %v (keepalives_ignored=%d)\n", err, keepalives)
			return 1
		}

		var env lightEnvelope
		if err := json.Unmarshal(frame, &env); err != nil {
			fmt.Printf("FRAME IGNORED unparseable (%d bytes)\n", len(frame))
			continue
		}
		if classifyInbound(env.Type, env.RequestID, req.MessageID) == frameIgnore {
			if env.Type == mesh.TypeKeepalive {
				keepalives++
				fmt.Printf("KEEPALIVE IGNORED message_id=%s (awaiting RESPONSE request_id=%s)\n",
					env.MessageID, req.MessageID)
			} else {
				fmt.Printf("FRAME IGNORED type=%s message_id=%s request_id=%s (awaiting %s)\n",
					env.Type, env.MessageID, env.RequestID, req.MessageID)
			}
			continue
		}

		if env.Type == mesh.TypeError {
			var errMsg mesh.ErrorMessage
			if err := json.Unmarshal(frame, &errMsg); err != nil {
				fmt.Printf("ROUNDTRIP FAIL uncodable ERROR frame: %v\n", err)
				return 1
			}
			fmt.Printf("ROUNDTRIP FAIL ERROR RECEIVED request_id=%s code=%s message=%q\n",
				errMsg.RequestID, errMsg.Error.Code, errMsg.Error.Message)
			return 1
		}
		if err := json.Unmarshal(frame, &resp); err != nil {
			fmt.Printf("ROUNDTRIP FAIL correlated frame does not decode as a RESPONSE: %v\n", err)
			return 1
		}
		break
	}

	fmt.Printf("RESPONSE RECEIVED request_id=%s response_message_id=%s status_code=%d body=%s\n",
		resp.RequestID, resp.MessageID, resp.StatusCode, string(resp.Body))
	if resp.RequestID != req.MessageID {
		// Unreachable through classifyInbound, kept explicit: the whole point of
		// the contract is that these two values are the same string.
		fmt.Printf("ROUNDTRIP FAIL RESPONSE.request_id=%s != REQUEST.message_id=%s\n",
			resp.RequestID, req.MessageID)
		return 1
	}
	if resp.StatusCode != *expectStatus {
		fmt.Printf("ROUNDTRIP FAIL RESPONSE.status_code=%d, expected %d\n", resp.StatusCode, *expectStatus)
		return 1
	}

	if *keepaliveWait <= 0 {
		fmt.Printf("ROUNDTRIP OK request_id=%s status_code=%d response_message_id=%s keepalives_ignored=%d\n",
			resp.RequestID, resp.StatusCode, resp.MessageID, keepalives)
		return 0
	}

	// Optional live leg: keep the socket open until the server's own keepalive
	// loop sends a frame, so a transcript can show a real KEEPALIVE arriving on
	// this socket and being ignored rather than only describing the rule. The
	// server's interval is mesh.DefaultMeshConfig().KeepaliveInterval (30s,
	// internal/mesh/peer.go) — callers pass a bound larger than that.
	fmt.Printf("KEEPALIVE WAIT up to %s for one live server KEEPALIVE\n", *keepaliveWait)
	if err := conn.SetReadDeadline(time.Now().Add(*keepaliveWait)); err != nil {
		log.Printf("roundtrip: set read deadline: %v", err)
		return 1
	}
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("ROUNDTRIP FAIL no KEEPALIVE within %s (keepalives_ignored=%d) — the server sends one every keepalive_interval_ms\n",
				*keepaliveWait, keepalives)
			return 1
		}
		var env lightEnvelope
		if err := json.Unmarshal(frame, &env); err != nil {
			fmt.Printf("FRAME IGNORED unparseable (%d bytes)\n", len(frame))
			continue
		}
		if env.Type != mesh.TypeKeepalive {
			fmt.Printf("FRAME IGNORED type=%s message_id=%s (waiting for KEEPALIVE)\n", env.Type, env.MessageID)
			continue
		}
		keepalives++
		fmt.Printf("KEEPALIVE OBSERVED message_id=%s agent_id=%s keepalives_ignored=%d\n",
			env.MessageID, keepaliveAgentID(frame), keepalives)
		fmt.Printf("ROUNDTRIP OK request_id=%s status_code=%d response_message_id=%s keepalives_ignored=%d\n",
			resp.RequestID, resp.StatusCode, resp.MessageID, keepalives)
		return 0
	}
}

// keepaliveAgentID reports the `agent_id` of a KEEPALIVE frame (the sender's
// identity: the server's mesh identity, or the peer's own id).
func keepaliveAgentID(frame []byte) string {
	var ka mesh.Keepalive
	if err := json.Unmarshal(frame, &ka); err != nil {
		return "?"
	}
	return ka.AgentID
}
