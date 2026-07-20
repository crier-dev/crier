package mesh

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type DialerConfig struct {
	HandshakeTimeout    time.Duration
	ReconnectBackoff    time.Duration
	MaxReconnectBackoff time.Duration
	MaxRetries          int
}

// DefaultDialerConfig returns a DialerConfig with sensible defaults:
// 10s handshake timeout, 1s→30s exponential reconnect backoff, 10 max retries.
func DefaultDialerConfig() DialerConfig {
	return DialerConfig{
		HandshakeTimeout:    10 * time.Second,
		ReconnectBackoff:    1 * time.Second,
		MaxReconnectBackoff: 30 * time.Second,
		MaxRetries:          10,
	}
}

type PeerConnection struct {
	PeerID  string
	URL     string
	conn    *websocket.Conn
	mu      sync.Mutex
	dialer  *websocket.Dialer
	config  DialerConfig
	done    chan struct{}
	onMsg   func([]byte)
	onClose func(error)
}

// NewPeerConnection creates a PeerConnection for the given peer URL.
// Call Connect(ctx) to establish the actual WebSocket connection.
func NewPeerConnection(peerID, rawURL string, config DialerConfig) *PeerConnection {
	return &PeerConnection{
		PeerID: peerID,
		URL:    rawURL,
		dialer: websocket.DefaultDialer,
		config: config,
		done:   make(chan struct{}),
	}
}

func (pc *PeerConnection) Connect(ctx context.Context) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.conn != nil {
		return fmt.Errorf("already connected to %s", pc.PeerID)
	}
	u, err := url.Parse(pc.URL)
	if err != nil {
		return fmt.Errorf("invalid peer URL %s: %w", pc.URL, err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, pc.config.HandshakeTimeout)
	defer cancel()
	conn, _, err := pc.dialer.DialContext(dialCtx, u.String(), http.Header{})
	if err != nil {
		return fmt.Errorf("dial peer %s: %w", pc.PeerID, err)
	}
	pc.conn = conn
	go pc.readLoop()
	return nil
}

func (pc *PeerConnection) Send(data []byte) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.conn == nil {
		return fmt.Errorf("not connected to %s", pc.PeerID)
	}
	if err := pc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return pc.conn.WriteMessage(websocket.TextMessage, data)
}

func (pc *PeerConnection) Close() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	select {
	case <-pc.done:
		return nil
	default:
		close(pc.done)
	}
	if pc.conn != nil {
		msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown")
		_ = pc.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(5*time.Second))
		return pc.conn.Close()
	}
	return nil
}

func (pc *PeerConnection) OnMessage(fn func([]byte)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onMsg = fn
}

func (pc *PeerConnection) OnClose(fn func(error)) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.onClose = fn
}

// NewAcceptedPeerConnection wraps an already-upgraded WebSocket connection.
// Used by the server when accepting incoming peer connections.
func NewAcceptedPeerConnection(peerID string, conn *websocket.Conn) *PeerConnection {
	return &PeerConnection{
		PeerID: peerID,
		conn:   conn,
		done:   make(chan struct{}),
	}
}

// StartReadLoop begins the read pump for an accepted connection.
func (pc *PeerConnection) StartReadLoop() {
	go pc.readLoop()
}

func (pc *PeerConnection) readLoop() {
	defer func() {
		pc.mu.Lock()
		if pc.conn != nil {
			pc.conn.Close()
			pc.conn = nil
		}
		pc.mu.Unlock()
	}()
	for {
		select {
		case <-pc.done:
			return
		default:
		}
		_, msg, err := pc.conn.ReadMessage()
		if err != nil {
			pc.mu.Lock()
			fn := pc.onClose
			pc.mu.Unlock()
			if fn != nil {
				fn(err)
			}
			return
		}
		pc.mu.Lock()
		fn := pc.onMsg
		pc.mu.Unlock()
		if fn != nil {
			fn(msg)
		}
	}
}
