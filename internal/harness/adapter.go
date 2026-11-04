// Package harness delivers one courier message into one live agent harness
// session (specs/COURIER-HARNESS.md).
//
// The package is the push half of Courier: the inbox stays first-class, and an
// adapter is the only thing that knows how to hand a message to a specific
// harness binary. Everything an adapter may execute is listed in the spec's
// verified-surfaces table (§2) — no adapter opens a TUI, drives a pty or
// scrapes a screen, and no adapter interpolates message text into a shell
// command (the message is attacker-influenced by definition).
package harness

import (
	"context"
	"time"
)

// Delivery states, exactly three (§3). A receipt says what happened, not what
// was intended.
const (
	// StateDispatched: the process was launched and exited 0; the harness has
	// the message.
	StateDispatched = "dispatched"
	// StateAccepted: a server-mode acknowledgement (opencode --attach, the
	// Hermes gateway) returned 2xx without a local process owning the turn.
	StateAccepted = "accepted"
	// StateFailed: launch/exit failed, or the harness rejected the session id.
	StateFailed = "failed"
)

// Error codes (§5). They are the machine-readable half of Receipt.Error; the
// human half lives in Receipt.Tail and in the registry's log line.
const (
	CodeUnknown         = "HARNESS_UNKNOWN"
	CodeUnavailable     = "HARNESS_UNAVAILABLE"
	CodeSessionUnknown  = "HARNESS_SESSION_UNKNOWN"
	CodeTimeout         = "HARNESS_TIMEOUT"
	CodeExitNonZero     = "HARNESS_EXIT_NONZERO"
	CodeOutputInvalid   = "HARNESS_OUTPUT_INVALID"
	CodeDisabled        = "HARNESS_DISABLED"
	CodeConcurrencyFull = "HARNESS_CONCURRENCY_FULL"
	// CodeWorkdirMissing and CodeInterrupted are named in §6; they are not in
	// the §5 catalog table but carry the same shape.
	CodeWorkdirMissing = "HARNESS_WORKDIR_MISSING"
	CodeInterrupted    = "HARNESS_INTERRUPTED"
)

// Adapter delivers one message into one harness session.
type Adapter interface {
	// Name is the stable harness id used in config, receipts and metrics.
	Name() string

	// Probe reports whether this adapter can run here. It must not start a
	// session: it inspects the binary path, version and required flags.
	Probe(ctx context.Context) (Capability, error)

	// Deliver sends msg into session sess. When sess.ID is empty the adapter
	// creates a session and reports its id; when set, it continues that one
	// and MUST fail (never silently create a different session) if the
	// harness does not know it.
	//
	// The returned error is reserved for a call that could not be attempted
	// at all; every harness-side outcome is carried by the Receipt with
	// State=failed and Error set to one of the codes above.
	Deliver(ctx context.Context, sess SessionRef, msg Message) (Receipt, error)
}

// Capability is what Probe measured. Available is the only field a caller may
// act on; Reason explains an unavailable adapter in words an operator can fix.
type Capability struct {
	Available bool            `json:"available"`
	Version   string          `json:"version,omitempty"`
	Binary    string          `json:"binary,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Features  map[string]bool `json:"features,omitempty"`
}

// SessionRef names the harness session a message belongs to. An empty ID means
// "create one"; a set ID means "continue exactly this one".
type SessionRef struct {
	Harness string   `json:"harness"`
	ID      string   `json:"id,omitempty"`
	Workdir string   `json:"workdir,omitempty"`
	Model   string   `json:"model,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// Message is the courier message as the adapter sees it. Text is untrusted
// input and is always passed as ONE argv element (or one HTTP field), never
// through a shell.
type Message struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Tags      []string `json:"tags,omitempty"`
	Author    string   `json:"author"`
	SessionID string   `json:"session_id"`
}

// Receipt is evidence, not a promise. It is persisted with the message.
type Receipt struct {
	Harness     string    `json:"harness"`
	SessionID   string    `json:"session_id"`
	Created     bool      `json:"created"`
	State       string    `json:"state"`
	ExitCode    int       `json:"exit_code,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	StdoutBytes int       `json:"stdout_bytes"`
	Tail        string    `json:"tail,omitempty"`
	Error       string    `json:"error,omitempty"`
	At          time.Time `json:"at"`
}

// Failed reports whether the receipt records a failure.
func (r Receipt) Failed() bool { return r.State == StateFailed }

// FailedReceipt builds a failure receipt. Adapters use it for every code in
// §5 so the vocabulary stays in one place.
func FailedReceipt(name, code, detail string) Receipt {
	rc := Receipt{
		Harness: name,
		State:   StateFailed,
		Error:   code,
		At:      time.Now().UTC(),
	}
	rc.Tail = detail
	return rc
}

// withTime stamps the receipt's completion time.
func (r Receipt) withTime(at time.Time) Receipt {
	r.At = at.UTC()
	return r
}
