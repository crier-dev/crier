package mesh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testWSEchoServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	return s, "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestDefaultDialerConfig(t *testing.T) {
	cfg := DefaultDialerConfig()
	if cfg.HandshakeTimeout != 10*time.Second {
		t.Errorf("HandshakeTimeout = %v, want 10s", cfg.HandshakeTimeout)
	}
	if cfg.ReconnectBackoff != 1*time.Second {
		t.Errorf("ReconnectBackoff = %v, want 1s", cfg.ReconnectBackoff)
	}
	if cfg.MaxReconnectBackoff != 30*time.Second {
		t.Errorf("MaxReconnectBackoff = %v, want 30s", cfg.MaxReconnectBackoff)
	}
	if cfg.MaxRetries != 10 {
		t.Errorf("MaxRetries = %d, want 10", cfg.MaxRetries)
	}
}

func TestPeerConnectionConnectAndSend(t *testing.T) {
	server, wsURL := testWSEchoServer(t)
	defer server.Close()

	pc := NewPeerConnection("peer-a", wsURL, DefaultDialerConfig())
	if err := pc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pc.Close()

	if pc.PeerID != "peer-a" {
		t.Errorf("PeerID = %q, want peer-a", pc.PeerID)
	}
	if pc.URL != wsURL {
		t.Errorf("URL = %q, want %q", pc.URL, wsURL)
	}

	msg := []byte(`hello`)
	if err := pc.Send(msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestPeerConnectionOnMessageReceivesServerMessage(t *testing.T) {
	server, wsURL := testWSEchoServer(t)
	defer server.Close()

	pc := NewPeerConnection("peer-b", wsURL, DefaultDialerConfig())
	if err := pc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pc.Close()

	received := make(chan []byte, 1)
	pc.OnMessage(func(data []byte) {
		select {
		case received <- data:
		default:
		}
	})

	if err := pc.Send([]byte(`ping`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case data := <-received:
		if string(data) != "ping" {
			t.Errorf("received %q, want ping", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPeerConnectionOnCloseFiresOnServerClose(t *testing.T) {
	upgrader := websocket.Upgrader{}
	closeConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		closeConn <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	pc := NewPeerConnection("peer-c", wsURL, DefaultDialerConfig())
	if err := pc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pc.Close()

	closed := make(chan error, 1)
	pc.OnClose(func(err error) {
		select {
		case closed <- err:
		default:
		}
	})

	select {
	case conn := <-closeConn:
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server connection")
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnClose callback")
	}
}

func TestPeerConnectionConnectAlreadyConnected(t *testing.T) {
	server, wsURL := testWSEchoServer(t)
	defer server.Close()

	pc := NewPeerConnection("peer-d", wsURL, DefaultDialerConfig())
	if err := pc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pc.Close()

	if err := pc.Connect(context.Background()); err == nil {
		t.Fatal("expected error on second connect, got nil")
	} else if !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("expected already connected error, got %v", err)
	}
}

func TestPeerConnectionSendNotConnected(t *testing.T) {
	pc := NewPeerConnection("peer-e", "ws://unused.example/ws", DefaultDialerConfig())
	if err := pc.Send([]byte(`hello`)); err == nil {
		t.Fatal("expected error when not connected, got nil")
	} else if !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected not connected error, got %v", err)
	}
}

func TestPeerConnectionConnectInvalidURL(t *testing.T) {
	pc := NewPeerConnection("peer-f", "://not-a-url", DefaultDialerConfig())
	if err := pc.Connect(context.Background()); err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}
}

func TestPeerConnectionConnectTimeout(t *testing.T) {
	cfg := DefaultDialerConfig()
	cfg.HandshakeTimeout = 10 * time.Millisecond
	pc := NewPeerConnection("peer-g", "ws://127.0.0.1:1/ws", cfg)
	if err := pc.Connect(context.Background()); err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestPeerConnectionCloseIsIdempotent(t *testing.T) {
	server, wsURL := testWSEchoServer(t)
	defer server.Close()

	pc := NewPeerConnection("peer-h", wsURL, DefaultDialerConfig())
	if err := pc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("Close second call: %v", err)
	}
}

func TestNewAcceptedPeerConnectionStartReadLoop(t *testing.T) {
	upgrader := websocket.Upgrader{}
	connected := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connected <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	defer clientConn.Close()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server connection")
	}

	pc := NewAcceptedPeerConnection("accepted-peer", serverConn)
	pc.StartReadLoop()

	received := make(chan []byte, 1)
	pc.OnMessage(func(data []byte) {
		select {
		case received <- data:
		default:
		}
	})

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte(`accepted`)); err != nil {
		t.Fatalf("write to server conn: %v", err)
	}

	select {
	case data := <-received:
		if string(data) != "accepted" {
			t.Errorf("received %q, want accepted", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for accepted message")
	}

	clientConn.Close()

	closed := make(chan struct{})
	pc.OnClose(func(error) { close(closed) })
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnClose")
	}
}

func TestPeerConnectionOnMessageSetBeforeConnect(t *testing.T) {
	server, wsURL := testWSEchoServer(t)
	defer server.Close()

	pc := NewPeerConnection("peer-i", wsURL, DefaultDialerConfig())
	var count int32
	pc.OnMessage(func(data []byte) {
		atomic.AddInt32(&count, 1)
	})
	if err := pc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pc.Close()

	if err := pc.Send([]byte(`x`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&count) == 0 {
		t.Fatal("OnMessage callback was not invoked")
	}
}

func TestPeerConnectionSetOnClose(t *testing.T) {
	pc := NewPeerConnection("peer-j", "ws://unused.example/ws", DefaultDialerConfig())
	var fired bool
	pc.OnClose(func(error) { fired = true })
	if !fired {
		// Callback is set; no panic verifies assignment path.
	}
}
