package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// DOGFOOD-RELAY-1 regression suite.
//
// Every relay subscription frame must name the LITERAL published topic while
// carrying the published event unchanged, so a wildcard subscriber — which
// only knows its pattern — can still tell WHICH topic matched. The wire shape
// asserted here is exactly:
//
//	{"topic":"<literal published topic>","event":<event, verbatim>}
//
// The expected frames below are built from literals, never from the production
// builder, so these assertions can actually fail.

// wantFrame is the exact frame a subscriber must receive for a publish to topic
// carrying event (a raw JSON value spliced in verbatim).
func wantFrame(topic, event string) string {
	return `{"topic":"` + topic + `","event":` + event + `}`
}

// nextFrame reads one frame from a subscription channel, failing on timeout.
func nextFrame(t *testing.T, ch <-chan []byte, context string) []byte {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed while waiting for a frame", context)
		}
		return got
	case <-time.After(recvTimeout):
		t.Fatalf("%s: timeout waiting for a frame", context)
	}
	return nil
}

// TestPublishFrameCarriesLiteralTopicAndVerbatimEvent pins the channel-level
// contract for every JSON value shape an event can take: object, string,
// array, number and null. A wildcard subscription must receive the same frame
// as an exact one — the literal topic is always present — and the event must
// stay the JSON value the publisher sent, never a string-encoded copy of it.
func TestPublishFrameCarriesLiteralTopicAndVerbatimEvent(t *testing.T) {
	const topic = "dogfood.news"

	cases := []struct {
		name  string
		event string
	}{
		{"object", `{"from":"publisher","body":"hello bus"}`},
		{"string", `"hello bus"`},
		{"array", `[1,"two",{"three":3}]`},
		{"number", `42`},
		{"null", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(0)
			exact, unsubExact := r.Subscribe(topic)
			defer unsubExact()
			star, unsubStar := r.Subscribe("dogfood.*")
			defer unsubStar()
			deep, unsubDeep := r.Subscribe("dogfood.>")
			defer unsubDeep()
			other, unsubOther := r.Subscribe("other.*")
			defer unsubOther()

			if err := r.Publish(topic, json.RawMessage(tc.event)); err != nil {
				t.Fatalf("Publish: %v", err)
			}

			got := nextFrame(t, exact, "exact subscriber")
			if string(got) != wantFrame(topic, tc.event) {
				t.Fatalf("exact frame = %s, want %s", got, wantFrame(topic, tc.event))
			}

			// The frame must decode to the literal topic plus the event as its
			// own JSON value — the RawMessage bytes are unchanged, which is what
			// rules out a string-encoded ("double-encoded") event.
			var frame Frame
			if err := json.Unmarshal(got, &frame); err != nil {
				t.Fatalf("frame %s is not a JSON object: %v", got, err)
			}
			if frame.Topic != topic {
				t.Errorf("frame topic = %q, want %q", frame.Topic, topic)
			}
			if string(frame.Event) != tc.event {
				t.Errorf("frame event = %s, want the published bytes %s", frame.Event, tc.event)
			}
			var wantVal, gotVal any
			if err := json.Unmarshal([]byte(tc.event), &wantVal); err != nil {
				t.Fatalf("test event %s is not valid JSON: %v", tc.event, err)
			}
			if err := json.Unmarshal(frame.Event, &gotVal); err != nil {
				t.Fatalf("frame event %s is not valid JSON: %v", frame.Event, err)
			}
			if !reflect.DeepEqual(gotVal, wantVal) {
				t.Errorf("frame event value = %#v, want %#v", gotVal, wantVal)
			}
			if tc.name == "string" && strings.Contains(string(got), `\"`) {
				t.Errorf("string event was re-encoded instead of passed through: %s", got)
			}

			// Wildcard subscribers see the same frame: the literal topic is the
			// whole point of the change.
			for name, ch := range map[string][]byte{
				"dogfood.*": nextFrame(t, star, "dogfood.* subscriber"),
				"dogfood.>": nextFrame(t, deep, "dogfood.> subscriber"),
			} {
				if string(ch) != wantFrame(topic, tc.event) {
					t.Errorf("%s frame = %s, want %s", name, ch, wantFrame(topic, tc.event))
				}
			}

			// Non-matching pattern: never delivered, and never told the topic.
			mustNotReceive(t, other, "other.* must not receive "+topic)

			// Exactly once per matching subscription.
			mustNotReceive(t, exact, "duplicate delivery to the exact subscriber")
			mustNotReceive(t, star, "duplicate delivery to dogfood.*")
		})
	}
}

// TestHandleSubscribeFrameIsEnvelopedOverWebSocket drives the real HTTP
// upgrade: two live WebSocket subscribers (exact and wildcard) plus a
// non-matching one, and asserts the ACTUAL frames read off the sockets — not a
// helper's serialization — for an object event and a non-object event.
func TestHandleSubscribeFrameIsEnvelopedOverWebSocket(t *testing.T) {
	rly := New(0)
	router := mux.NewRouter()
	router.HandleFunc("/relay/subscribe/{topic}", rly.HandleSubscribe)
	router.HandleFunc("/relay/publish", rly.HandlePublish).Methods("POST")

	srv := httptest.NewServer(router)
	defer srv.Close()

	dial := func(pattern string) *websocket.Conn {
		t.Helper()
		url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay/subscribe/" + pattern
		conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %s: %v (status %v)", pattern, err, resp)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}

	readFrame := func(conn *websocket.Conn, context string) string {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("%s: read frame: %v", context, err)
		}
		return string(msg)
	}

	mustBeSilent := func(conn *websocket.Conn, context string) {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(dropTimeout))
		if _, msg, err := conn.ReadMessage(); err == nil {
			t.Fatalf("%s: unexpected frame %s", context, msg)
		}
	}

	publish := func(topic, event string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/relay/publish", strings.NewReader(
			`{"topic":"`+topic+`","event":`+event+`}`,
		))
		if err != nil {
			t.Fatalf("new publish request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("publish %s: %v", topic, err)
		}
		return resp
	}

	exact := dial("dogfood.news")
	star := dial("dogfood.*")
	other := dial("other.*")

	// Wait for the server-side registrations instead of sleeping: the handler
	// subscribes after the upgrade response reaches the client.
	deadline := time.Now().Add(2 * time.Second)
	for {
		registered := map[string]bool{}
		for _, ti := range rly.Topics() {
			registered[ti.Name] = true
		}
		if registered["dogfood.news"] && registered["dogfood.*"] && registered["other.*"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriptions not registered in time: %+v", rly.Topics())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Object event: the live frame read off the wildcard socket must name the
	// literal topic and carry the event as an object.
	resp := publish("dogfood.news", `{"from":"publisher","body":"hello bus"}`)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read publish response: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("publish status = %d, want 202", resp.StatusCode)
	}
	// The envelope is a WebSocket-frame change only: POST /relay/publish keeps
	// answering an empty 202.
	if len(body) != 0 {
		t.Errorf("POST /relay/publish 202 body = %q, want empty", body)
	}

	wantObject := wantFrame("dogfood.news", `{"from":"publisher","body":"hello bus"}`)
	if got := readFrame(star, "dogfood.*"); got != wantObject {
		t.Errorf("wildcard frame = %s, want %s", got, wantObject)
	}
	if got := readFrame(exact, "dogfood.news"); got != wantObject {
		t.Errorf("exact frame = %s, want %s", got, wantObject)
	}

	// Non-object event: the event must remain its own JSON value in the frame.
	resp = publish("dogfood.news", `["a",null,3]`)
	resp.Body.Close()
	wantArray := wantFrame("dogfood.news", `["a",null,3]`)
	for name, conn := range map[string]*websocket.Conn{"dogfood.*": star, "dogfood.news": exact} {
		if got := readFrame(conn, name); got != wantArray {
			t.Errorf("%s frame = %s, want %s", name, got, wantArray)
		}
	}

	// Exactly one frame per publish per subscription, and nothing at all for a
	// topic the pattern does not match. These are the LAST reads on each socket:
	// a gorilla read deadline that expires poisons the connection for reads.
	mustBeSilent(other, "other.* subscriber")
	mustBeSilent(star, "dogfood.* duplicate frame")
	mustBeSilent(exact, "dogfood.news duplicate frame")
}
