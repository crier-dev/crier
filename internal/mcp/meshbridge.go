package mcp

// meshBridge is the live-messaging half of the crier-mcp bridge. It owns one
// WebSocket connection to the Crier server's mesh endpoint (identity = the
// bridge's agent id) and exposes a synchronous request/response primitive to
// the MCP tools, implementing the correlation contract from docs/mesh-protocol.md:
//
//   - the REQUEST carries a fresh message_id; the RESPONSE must echo it as
//     request_id, otherwise the server silently drops it
//   - ERROR frames are surfaced as errors with their code/message
//
// Incoming REQUESTs (another agent asking the bridge's agent something over
// the mesh) are auto-answered with a bridge-alive response — the durable
// inbox is the lane for LLM content (see ask_agent/get_messages), so this is
// a liveness probe, not a content channel. The connection is created lazily
// on the first mesh_request call and kept for the lifetime of the process
// (the protocol has no automatic reconnect; a dead socket is closed for good).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/crier/internal/mesh"
)

type meshReply struct {
	status int
	body   json.RawMessage
	trace  string
	err    *meshReplyErr
}

type meshReplyErr struct {
	code    string
	message string
}

type meshBridge struct {
	agentID string
	url     string

	mu      sync.Mutex
	conn    *mesh.PeerConnection
	pending map[string]chan meshReply
}

func newMeshBridge(agentID, url string) *meshBridge {
	return &meshBridge{
		agentID: agentID,
		url:     url,
		pending: make(map[string]chan meshReply),
	}
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// request sends a REQUEST frame and blocks until the correlated RESPONSE
// (or ERROR) arrives, or the timeout elapses.
func (b *meshBridge) request(target, method, path string, body any, timeoutMs int) (meshReply, error) {
	if timeoutMs <= 0 {
		timeoutMs = 15000
	}
	if err := b.ensureConnected(); err != nil {
		return meshReply{}, err
	}
	mid := randomID()
	ch := make(chan meshReply, 1)
	b.mu.Lock()
	b.pending[mid] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, mid)
		b.mu.Unlock()
	}()

	req := mesh.Request{
		Envelope: mesh.Envelope{
			Type:      mesh.TypeRequest,
			Version:   1,
			MessageID: mid,
			Timestamp: time.Now(),
		},
		Source:    mesh.PeerRef{AgentID: b.agentID},
		Target:    mesh.PeerRef{AgentID: target},
		Method:    method,
		Path:      path,
		Body:      body,
		TraceID:   mid,
		TimeoutMs: timeoutMs,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return meshReply{}, fmt.Errorf("encode mesh request: %w", err)
	}
	b.mu.Lock()
	err = b.conn.Send(raw)
	b.mu.Unlock()
	if err != nil {
		return meshReply{}, fmt.Errorf("send mesh request: %w", err)
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			return reply, fmt.Errorf("[%s] %s", reply.err.code, reply.err.message)
		}
		return reply, nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return meshReply{}, fmt.Errorf("mesh timeout: no RESPONSE for %s within %dms", mid, timeoutMs)
	}
}

func (b *meshBridge) ensureConnected() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return nil
	}
	conn := mesh.NewPeerConnection(b.agentID, b.url, mesh.DefaultDialerConfig())
	conn.OnMessage(b.onFrame)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Connect(ctx); err != nil {
		return fmt.Errorf("mesh connect: %w", err)
	}
	// One-way REGISTER immediately after upgrade (no REGISTER_ACK exists).
	reg, err := json.Marshal(mesh.Register{
		Envelope: mesh.Envelope{
			Type:      mesh.TypeRegister,
			Version:   1,
			MessageID: randomID(),
			Timestamp: time.Now(),
		},
		AgentID:    b.agentID,
		LeaseID:    "",
		LeaseTTLMs: 3600000,
		Capabilities: mesh.Capabilities{
			Version:               "0.1.0",
			Topics:                []string{},
			MaxConcurrentSessions: 4,
		},
	})
	if err != nil {
		conn.Close()
		return fmt.Errorf("encode register: %w", err)
	}
	if err := conn.Send(reg); err != nil {
		conn.Close()
		return fmt.Errorf("mesh register: %w", err)
	}
	b.conn = conn
	return nil
}

// connectWithRetry establishes the mesh connection at startup so the bridge
// is visible to /mesh/peers and reachable by other peers (the protocol has no
// automatic reconnect, but startup retries tolerate the server not being up
// yet).
func (b *meshBridge) connectWithRetry(attempts int, delay time.Duration) {
	for i := 0; i < attempts; i++ {
		if err := b.ensureConnected(); err == nil {
			return
		} else if i < attempts-1 {
			time.Sleep(delay)
		}
	}
}

// onFrame runs on the connection's read loop.
func (b *meshBridge) onFrame(data []byte) {
	var env mesh.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return // malformed frames are silently dropped per protocol
	}
	switch env.Type {
	case mesh.TypeResponse:
		var resp mesh.Response
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}
		b.resolve(resp.RequestID, meshReply{status: resp.StatusCode, body: resp.Body, trace: resp.TraceID})
	case mesh.TypeError:
		var errFrame struct {
			mesh.Envelope
			RequestID string `json:"request_id"`
			Error     struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &errFrame); err != nil {
			return
		}
		b.resolve(errFrame.RequestID, meshReply{err: &meshReplyErr{
			code: errFrame.Error.Code, message: errFrame.Error.Message,
		}})
	case mesh.TypeRequest:
		// Auto-respond: the mesh is the liveness lane, the inbox is the
		// content lane for LLM traffic.
		var req mesh.Request
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		resp, _ := json.Marshal(mesh.Response{
			Envelope: mesh.Envelope{
				Type:      mesh.TypeResponse,
				Version:   1,
				MessageID: randomID(),
				Timestamp: time.Now(),
			},
			RequestID:  req.MessageID,
			Source:     mesh.PeerRef{AgentID: b.agentID},
			StatusCode: 200,
			Body:       json.RawMessage(`{"status":"bridge_alive","note":"use the inbox for LLM content"}`),
			TraceID:    req.TraceID,
		})
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.conn != nil {
			_ = b.conn.Send(resp)
		}
	default:
		// KEEPALIVE / REGISTER_ACK: ignored per protocol.
	}
}

func (b *meshBridge) resolve(requestID string, reply meshReply) {
	b.mu.Lock()
	ch := b.pending[requestID]
	b.mu.Unlock()
	if ch != nil {
		ch <- reply
	}
}
