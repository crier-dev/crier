package relay

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Subscriber liveness accounting (QA-CRIER-43).
//
// The relay cuts a subscriber whose read deadline expires without an inbound
// pong (QA-CRIER-35). That is the documented half-open reap, but a bare
// subscriber COUNT cannot tell the two shapes apart:
//
//   - the peer really went silent — the reap is the product working, and
//   - the read pump was never scheduled, so pongs the peer DID send sat unread
//     in the socket buffer until the wall-clock deadline expired — the reap is
//     the host starving the loop, not the product misbehaving.
//
// Both look identical from the outside ("count went to 0"), which is why a
// control test that pins a sub-second silence bound can redden on a busy box
// while the same HEAD is green on an idle one. This file makes the difference
// observable: every reap records WHY the read pump stopped and the liveness
// counters that were in force at the moment it stopped, so a failing test (or
// an operator reading the logs) quotes the reaper's own verdict instead of
// guessing. It never suppresses a reap.

// ReapReason classifies why a subscription's read pump stopped — the reaper's
// own verdict, not an inference from the subscriber count.
type ReapReason string

const (
	// ReapSilence is the read deadline expiring: no pong arrived within the
	// relay's silence bound. This is the documented half-open / laptop-sleep
	// reap. It is also the shape a descheduled read pump produces, which is
	// exactly why the counters below are recorded alongside it.
	ReapSilence ReapReason = "silence-bound"
	// ReapPeerClose is the peer hanging up: a WebSocket close frame, or a
	// clean/abrupt end of stream on the socket.
	ReapPeerClose ReapReason = "peer-close"
	// ReapReadError is any other read failure (reset, protocol violation).
	ReapReadError ReapReason = "read-error"
)

// keepaliveCounters is one subscription's liveness accounting: pings the relay
// put on the wire and pongs the read pump actually processed. The pair is the
// discriminator — "pings sent 12, pongs seen 0" on a peer that answered every
// ping means the pump never ran, while "pings sent 12, pongs seen 12" with a
// silence reap would mean the deadline extension itself is broken.
//
// They are atomics because the ping pump writes pings, the read pump writes
// pongs, and the read pump READS both as it records the reap.
type keepaliveCounters struct {
	pingsSent atomic.Int64
	pongsSeen atomic.Int64
}

// KeepaliveReap is the record of one ended subscription.
type KeepaliveReap struct {
	Topic     string
	AgentID   string
	Namespace string
	// Reason is the reaper's classification of the terminating read error.
	Reason ReapReason
	// Detail is the terminating error exactly as the read pump saw it, so an
	// unexpected reap can be read without reproducing it.
	Detail string
	// PongWait is the silence bound that was in force for this subscription —
	// a reap is only interpretable against the deadline it beat.
	PongWait time.Duration
	// PingsSent and PongsSeen are the liveness counters at reap time.
	PingsSent int64
	PongsSeen int64
	// Lifetime is how long the subscription lived before it was reaped.
	Lifetime time.Duration
	// At is when the reap was recorded.
	At time.Time
}

// String renders one reap as a single line for logs and test failure messages.
func (k KeepaliveReap) String() string {
	return fmt.Sprintf("topic=%s agent=%s reason=%s detail=%q pong_wait=%v pings_sent=%d pongs_seen=%d lived=%v",
		k.Topic, k.AgentID, k.Reason, k.Detail, k.PongWait, k.PingsSent, k.PongsSeen, k.Lifetime)
}

// maxReapRecords bounds the per-relay reap ledger: enough to explain the
// reaps around a failure, few enough that a long-lived relay cannot grow one
// record per disconnected subscriber forever.
const maxReapRecords = 32

// classifyReapReason names why a read pump stopped, from the error it returned.
func classifyReapReason(err error) ReapReason {
	if err == nil {
		return ReapReadError
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ReapSilence
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return ReapPeerClose
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ReapPeerClose
	}
	return ReapReadError
}

// maxReapDetail bounds the recorded error text: a reap record is read by
// humans and by test failure messages, and a socket error can carry a long
// address form that adds nothing past its first line.
const maxReapDetail = 200

// reapDetail renders the terminating read error for the record, bounded.
func reapDetail(err error) string {
	if err == nil {
		return "read pump exited without an error"
	}
	detail := err.Error()
	if len(detail) > maxReapDetail {
		detail = detail[:maxReapDetail] + "…"
	}
	return detail
}

// recordReap appends one reap to the relay's bounded ledger, oldest first.
func (r *Relay) recordReap(rec KeepaliveReap) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reaps = append(r.reaps, rec)
	if len(r.reaps) > maxReapRecords {
		r.reaps = append([]KeepaliveReap(nil), r.reaps[len(r.reaps)-maxReapRecords:]...)
	}
}

// Reaps returns a copy of the most recent reap records, oldest first. It is the
// read side of the liveness ledger: a test or an operator can ask the relay why
// a subscriber disappeared instead of inferring it from a count.
func (r *Relay) Reaps() []KeepaliveReap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.reaps) == 0 {
		return nil
	}
	out := make([]KeepaliveReap, len(r.reaps))
	copy(out, r.reaps)
	return out
}

// LastReap returns the most recent reap record, if any.
func (r *Relay) LastReap() (KeepaliveReap, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.reaps) == 0 {
		return KeepaliveReap{}, false
	}
	return r.reaps[len(r.reaps)-1], true
}
