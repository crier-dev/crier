// Package claudecode adapts the Anthropic Claude Code CLI (CRIER-HARNESS §2).
//
// Two verified surfaces, nothing else:
//   - create:   `claude -p --output-format json "<text>"`
//   - continue: `claude -p --resume <id> --output-format json "<text>"`
//
// Both are documented by the installed binary (2.1.318): `-p/--print` is the
// one non-interactive turn, `--resume <session-id>` continues an existing
// session, and `--output-format json` answers a single JSON result object that
// carries `session_id`. Claude Code has no create-with-a-chosen-id flag, so the
// adapter always takes the id the CLI minted — the same shape codex uses.
//
// A `--resume` of an id the CLI does not have is NOT turned into a create: it
// is reported as HARNESS_SESSION_UNKNOWN, because a silent create would fork
// the transcript (§4.3).
//
// The permission mode is passed explicitly when configured, and left alone when
// it is not: a message delivery must not silently widen an operator's posture.
// With `-p` and no mode set, tool use is refused rather than prompted — the
// message still arrives, which is what delivery means.
//
// Text over 64 KiB goes on stdin (`-p` reads the prompt from stdin when no
// positional is given), so ARG_MAX never caps a Crier message (§6).
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// Name is the stable harness id.
const Name = "claude-code"

// maxArgBytes is where the message stops travelling in the argument vector.
const maxArgBytes = 64 << 10

// result is the subset of `--output-format json` this adapter reads. The CLI
// emits one object; the fields below are the ones a delivery needs.
type result struct {
	SessionID string `json:"session_id"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
}

// Config is adapters.claude-code (§4.1).
type Config struct {
	Binary  string
	Workdir string
	Model   string
	// PermissionMode is passed as --permission-mode when set (acceptEdits,
	// bypassPermissions, manual, …). Empty means: do not touch the operator's
	// posture.
	PermissionMode string
	// Timeout bounds one delivery; zero takes the harness default.
	Timeout time.Duration
}

// Adapter delivers messages into Claude Code sessions.
type Adapter struct {
	cfg Config
	now func() time.Time
}

// New builds the adapter. An empty binary means "claude" on PATH.
func New(cfg Config) *Adapter {
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = "claude"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = harness.DefaultTimeout
	}
	return &Adapter{cfg: cfg, now: time.Now}
}

// Name is the harness id used in config and receipts, and the harness label
// value on crier_harness_dispatches_total, crier_harness_dispatch_errors_total
// and crier_harness_dispatch_duration_seconds (CRIER-OBS-1).
func (a *Adapter) Name() string { return Name }

// Features reports the surfaces this adapter uses: `print` (-p, one turn),
// `resume` (continue), `json` (the session id comes from the result object),
// and the two pass-through knobs it actually honours.
func (a *Adapter) Features() map[string]bool {
	return map[string]bool{
		"continue":   true,
		"create":     true,
		"resume":     true,
		"print":      true,
		"json":       true,
		"model":      strings.TrimSpace(a.cfg.Model) != "",
		"permission": strings.TrimSpace(a.cfg.PermissionMode) != "",
		"stdin":      true,
	}
}

// Probe measures the binary and its version. It never starts a session and
// never pre-flights credentials: a logged-out CLI is available, and Deliver
// carries its own stderr tail (§6).
func (a *Adapter) Probe(ctx context.Context) (harness.Capability, error) {
	return harness.ProbeBinary(ctx, a.cfg.Binary, a.Features())
}

// Deliver creates (empty sess.ID) or continues a Claude Code session.
func (a *Adapter) Deliver(ctx context.Context, sess harness.SessionRef, msg harness.Message) (harness.Receipt, error) {
	start := a.now()
	workdir := sess.Workdir
	if workdir == "" {
		workdir = a.cfg.Workdir
	}
	if err := harness.WorkdirError(workdir); err != nil {
		return harness.FailedReceipt(Name, harness.CodeWorkdirMissing, "harness workdir "+workdir+": "+err.Error()), nil
	}
	path, err := harness.LookupBinary(a.cfg.Binary)
	if err != nil {
		return harness.FailedReceipt(Name, harness.CodeUnavailable, "binary not found: "+a.cfg.Binary), nil
	}

	id := strings.TrimSpace(sess.ID)
	args, stdin := a.argv(sess, msg, id)
	res := harness.RunCommand(ctx, harness.ExecSpec{
		Path:  path,
		Args:  args,
		Dir:   workdir,
		Stdin: stdin,
	}, a.cfg.Timeout)

	rc := harness.Receipt{
		Harness:     Name,
		SessionID:   id,
		Created:     id == "",
		State:       harness.StateDispatched,
		ExitCode:    res.ExitCode,
		DurationMS:  time.Since(start).Milliseconds(),
		StdoutBytes: res.StdoutSize,
		At:          a.now().UTC(),
	}
	parsed := ParseResult(res.Stdout)
	if id == "" && parsed.SessionID != "" {
		// Take the minted id even from a failed turn: a session that was
		// created and then failed is resumable, and the receipt is the only
		// place Crier can learn its id.
		rc.SessionID = parsed.SessionID
	}
	switch {
	case res.StartError != "":
		return harness.FailedReceipt(Name, harness.CodeUnavailable, res.StartError), nil
	case res.TimedOut:
		rc.State = harness.StateFailed
		rc.Error = harness.CodeTimeout
		rc.Tail = fmt.Sprintf("delivery exceeded %s", a.cfg.Timeout)
		return rc, nil
	case res.ExitCode != 0:
		rc.State = harness.StateFailed
		rc.Error = harness.ClassifyFailure(string(res.Stdout), string(res.Stderr))
		rc.Tail = res.Tail
		return rc, nil
	case parsed.IsError:
		// The CLI answered exit 0 but reported the turn as an error: report the
		// harness's own words rather than a success the transcript denies.
		rc.State = harness.StateFailed
		rc.Error = harness.ClassifyFailure(string(res.Stdout), string(res.Stderr))
		rc.Tail = firstLine(parsed.Result)
		return rc, nil
	}

	if rc.Created && rc.SessionID == "" {
		// Delivered but unbound: the Crier session needs the id the CLI minted,
		// so the receipt downgrades to OUTPUT_INVALID exactly as §5 requires.
		rc.Error = harness.CodeOutputInvalid
		rc.Tail = firstLine(string(res.Stdout))
	}
	return rc, nil
}

// argv builds the argument vector and the optional stdin body.
func (a *Adapter) argv(sess harness.SessionRef, msg harness.Message, id string) ([]string, string) {
	create := id == ""
	args := []string{"-p", "--output-format", "json"}
	if !create {
		args = append(args, "--resume", id)
	}
	if model := a.model(sess); model != "" {
		args = append(args, "--model", model)
	}
	if mode := strings.TrimSpace(a.cfg.PermissionMode); mode != "" {
		args = append(args, "--permission-mode", mode)
	}
	args = append(args, sess.Args...)
	if len(msg.Text) > maxArgBytes {
		return args, msg.Text
	}
	return append(args, msg.Text), ""
}

// model is the per-session model override, else the configured one.
func (a *Adapter) model(sess harness.SessionRef) string {
	if m := strings.TrimSpace(sess.Model); m != "" {
		return m
	}
	return strings.TrimSpace(a.cfg.Model)
}

// ParseResult reads the session id out of a `--output-format json` answer.
// Anything that does not parse (a stream-json mode, a pre-JSON crash) yields
// the zero value, and the caller reports what it then lacks rather than
// inventing an id.
func ParseResult(out []byte) result {
	var r result
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return r
	}
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return result{}
	}
	return r
}

// firstLine keeps a harness message to its first line so a receipt stays a
// status, not a log.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
