package mesh

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestResponseBodyRelayedVerbatim pins the DOGFOOD-MESH-2 wire truth: a RESPONSE
// `body` is an OPAQUE JSON value whose JSON type is chosen by the RESPONDER, and
// the server relays it verbatim. The server keeps the frame's raw bytes
// (`Response.Body json.RawMessage`, internal/mesh/message.go:79) and
// forwardResponse hands the bytes it received straight to the requester's
// connection (`conn.Send(data)`, internal/mesh/peer.go:402-420) — nothing
// unmarshals the body into a Go value and re-encodes it, so an object body
// arrives as an object and a string body arrives as a string.
//
// The test drives the REAL relayed path (two peers accepted into one Mesh, the
// same wiring as TestAgentToAgentRequestRouting) and answers each REQUEST with a
// hand-built frame, so the exact bytes of the body are the fixture's to choose —
// including the spacing and key order a server-side re-encode would destroy.
func TestResponseBodyRelayedVerbatim(t *testing.T) {
	cases := []struct {
		name string
		// body is the exact JSON text the responder puts on the wire.
		body string
		// want is the decoded value the requester must see, with the same JSON
		// type the responder sent.
		want any
	}{
		{
			// Object body: unsorted keys and loose spacing, so a server that
			// re-encoded through a Go map would sort the keys and compact the
			// whitespace (json.Marshal of map[string]any).
			name: "object_body_stays_object",
			body: `{ "pong" : true , "zeta" : 1 , "alpha" : 2 }`,
			want: map[string]any{"pong": true, "zeta": float64(1), "alpha": float64(2)},
		},
		{
			// String body: what a responder that stringifies its own payload
			// with json.dumps sends (examples/llm-mesh/mesh/crier_mesh.py:174).
			// The content carries `<` and `&` so a Go re-encode is detectable
			// by its \u003c / \u0026 escapes as well as by the type.
			name: "string_body_stays_string",
			body: `"{\"pong\": true, \"note\": \"a<b&c\"}"`,
			want: `{"pong": true, "note": "a<b&c"}`,
		},
		{
			// Array body: neither object nor string, spacing is the tell.
			name: "array_body_stays_array",
			body: `[ 3, 1, 2 ]`,
			want: []any{float64(3), float64(1), float64(2)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fixture must be DISCRIMINATING: if a server-side re-encode of
			// the decoded value reproduced the bytes we send, this test would
			// pass on any implementation and prove nothing.
			if reencoded, err := json.Marshal(tc.want); err == nil && string(reencoded) == tc.body {
				t.Fatalf("fixture not discriminating: json.Marshal of the decoded value reproduces the sent bytes %s", tc.body)
			}

			clientA, serverA := startAgentEndpoint(t)
			clientB, serverB := startAgentEndpoint(t)
			connA, connB := clientA(), clientB()

			m := NewMesh(DefaultMeshConfig("relay"))
			acceptAgent(t, m, "agent-a", connA, serverA())
			acceptAgent(t, m, "agent-b", connB, serverB())

			// agent-a (requester) sends a REQUEST addressed to agent-b.
			reqID := "req-verbatim-" + tc.name
			req := &Request{
				Envelope: Envelope{
					Type:      TypeRequest,
					Version:   1,
					MessageID: reqID,
					Timestamp: time.Now(),
				},
				Source:    PeerRef{AgentID: "agent-a"},
				Target:    PeerRef{AgentID: "agent-b"},
				Method:    "POST",
				Path:      "/echo",
				Body:      map[string]any{"echo": true},
				TraceID:   "trace-" + tc.name,
				TimeoutMs: 5000,
			}
			data, err := Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := connA.WriteMessage(websocket.TextMessage, data); err != nil {
				t.Fatal(err)
			}

			// agent-b receives the forwarded REQUEST through the server's router.
			_, reqEnv := readUntilType(t, connB, TypeRequest, 5*time.Second)
			if reqEnv.MessageID != reqID {
				t.Fatalf("forwarded REQUEST message_id = %q, want %q", reqEnv.MessageID, reqID)
			}

			// agent-b (responder) answers with a hand-built frame so the body's
			// bytes are exactly ours. The frame must be valid NDJSON exactly as
			// Marshal emits it: one object plus the trailing newline.
			sentBody := tc.body
			frame := fmt.Sprintf(
				`{"type":"RESPONSE","version":1,"message_id":"resp-%s","timestamp":"%s",`+
					`"request_id":"%s","source":{"agent_id":"agent-b"},"status_code":200,`+
					`"body":%s}`+"\n",
				tc.name, time.Now().Format(time.RFC3339Nano), reqID, sentBody)
			if !json.Valid([]byte(frame)) {
				t.Fatalf("test frame is not valid JSON: %s", frame)
			}
			if err := connB.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				t.Fatal(err)
			}

			// agent-a receives the RESPONSE that came back through the route
			// table. The server relays `data` verbatim, so the whole frame must
			// be byte-identical to what the responder wrote.
			msg, env := readUntilType(t, connA, TypeResponse, 5*time.Second)
			if env.MessageID != "resp-"+tc.name {
				t.Fatalf("relayed RESPONSE message_id = %q, want %q", env.MessageID, "resp-"+tc.name)
			}
			if string(msg) != frame {
				t.Fatalf("frame was not relayed verbatim:\n  sent: %s\n  got:  %s", frame, msg)
			}

			var resp Response
			if err := json.Unmarshal(msg, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.RequestID != reqID {
				t.Fatalf("response request_id = %q, want %q", resp.RequestID, reqID)
			}
			if got := string(resp.Body); got != sentBody {
				t.Fatalf("body bytes were not relayed verbatim (re-encoded?):\n  sent: %s\n  got:  %s", sentBody, got)
			}

			// Same JSON type and same value as the responder sent.
			var decoded any
			if err := json.Unmarshal(resp.Body, &decoded); err != nil {
				t.Fatalf("decode relayed body %s: %v", resp.Body, err)
			}
			if !reflect.DeepEqual(decoded, tc.want) {
				t.Fatalf("body type/value changed in relay: got %T %#v, want %T %#v",
					decoded, decoded, tc.want, tc.want)
			}
			if gotType, wantType := jsonTypeName(decoded), jsonTypeName(tc.want); gotType != wantType {
				t.Fatalf("body JSON type changed in relay: got %s, want %s", gotType, wantType)
			}

			// The bridge side of the SAME bytes: the mesh_request tool decodes
			// reply.body into map[string]any (internal/mcp/messaging.go:266-271,
			// internal/mcp/types.go:204-208) and discards the error, so only an
			// OBJECT body reaches an MCP client — a string body arrives there as
			// an empty body.
			var asMap map[string]any
			mapErr := json.Unmarshal(resp.Body, &asMap)
			_, wantObject := tc.want.(map[string]any)
			if wantObject && mapErr != nil {
				t.Fatalf("object body must decode into the bridge's map[string]any: %v", mapErr)
			}
			if !wantObject && mapErr == nil {
				t.Fatalf("expected the %s body to fail the bridge's map[string]any decode (so the bridge drops it), but it decoded to %v",
					tc.name, asMap)
			}
			// What the MCP client finally sees: the failed decode leaves the map
			// nil and the tool result marshals that field as JSON null.
			if !wantObject {
				out, err := json.Marshal(map[string]any{"body": asMap})
				if err != nil {
					t.Fatal(err)
				}
				if string(out) != `{"body":null}` {
					t.Fatalf("bridge output for a %s body = %s, want {\"body\":null}", tc.name, out)
				}
			}
		})
	}
}

// jsonTypeName names the JSON type of a decoded value, for failure messages.
func jsonTypeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case string:
		return "string"
	case []any:
		return "array"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}
