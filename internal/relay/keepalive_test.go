package relay

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// startSubscribeServer stands up a relay with the subscribe route mounted and
// its silence bound scaled to `wait`, so the reaper has to actually fire during
// the test instead of waiting out the 60s default.
func startSubscribeServer(t *testing.T, wait time.Duration) (*Relay, *httptest.Server, string) {
	t.Helper()
	rly := New(0)
	rly.SetPongWait(wait)
	router := mux.NewRouter()
	router.HandleFunc("/relay/subscribe/{topic}", rly.HandleSubscribe)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/relay/subscribe/agent.status"
	return rly, srv, wsURL
}

// waitCount polls SubscriberCount until it equals want or the budget passes.
func waitCount(t *testing.T, rly *Relay, want int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for rly.SubscriberCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber count = %d, want %d after %v",
				rly.SubscriberCount(), want, budget)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSubscribeSilentSubscriberReaped proves QA-CRIER-35: a subscriber that
// connects and then goes silent — never pongs, never sends a close frame, the
// socket left half-open — is eventually cut by the read deadline and its
// subscription unregistered. Before the fix this loop had no deadline at all,
// so a half-open subscriber stayed in r.subs (and its 64-slot event channel)
// forever and subscriber counts drifted.
//
// The client deliberately does NOT answer pings and does NOT close: that is
// exactly the laptop-sleep / NAT-drop shape, where no TCP FIN is ever sent.
func TestSubscribeSilentSubscriberReaped(t *testing.T) {
	const pongWait = 150 * time.Millisecond
	rly, _, wsURL := startSubscribeServer(t, pongWait)

	// Deliberately drop the connection handle: no pong handler, no read loop,
	// no Close. The client mimes a gone-silent peer — closing the raw socket
	// would send a FIN, which is the one thing the defect shape lacks.
	if _, _, err := websocket.DefaultDialer.Dial(wsURL, nil); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// FIRST the subscription must actually register. Without this pin the
	// cell is vacuous: the first count sample could observe 0 before the
	// server ever added the subscriber, "passing" without reaping anything.
	waitCount(t, rly, 1, pongWait)

	// THEN the silence bound must reap it. Well past the deadline
	// (wait = 150ms, ping period = 75ms), bounded so the test stays
	// -short-safe: worst case ~1.6s of polling, never the 60s default.
	waitCount(t, rly, 0, 10*pongWait)
}

// TestSubscribePongingSubscriberSurvives is the control: a live subscriber
// that answers every ping is still registered after many silence windows have
// passed. This is the cell that fails if anyone "fixes" the leak by refusing
// to keep subscribers alive at all.
func TestSubscribePongingSubscriberSurvives(t *testing.T) {
	const pongWait = 120 * time.Millisecond
	rly, _, wsURL := startSubscribeServer(t, pongWait)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData),
			time.Now().Add(time.Second))
	})
	// Drain inbound frames so control-frame reads keep flowing; reads also
	// drive the pong dispatch above.
	go func() {
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Eight silence windows: with a 60ms ping period the server pings ~16
	// times. A subscriber whose pongs did not extend the deadline would be
	// reaped after the first.
	time.Sleep(8 * pongWait)
	if got := rly.SubscriberCount(); got != 1 {
		t.Fatalf("ponging subscriber was reaped: count = %d, want 1", got)
	}
}

// TestSubscribePongWaitDefault pins the configured default: a fresh relay has
// the production silence bound, and SetPongWait(0) restores it rather than
// disabling the bound.
func TestSubscribePongWaitDefault(t *testing.T) {
	rly := New(0)
	if got := rly.PongWait(); got != defaultPongWait {
		t.Fatalf("default PongWait = %v, want %v", got, defaultPongWait)
	}
	rly.SetPongWait(time.Second)
	if got := rly.PongWait(); got != time.Second {
		t.Fatalf("after SetPongWait: PongWait = %v, want 1s", got)
	}
	rly.SetPongWait(0)
	if got := rly.PongWait(); got != defaultPongWait {
		t.Fatalf("zero override must restore default: PongWait = %v, want %v", got, defaultPongWait)
	}
}
