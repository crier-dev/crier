// Package opencode adapts the opencode CLI (COURIER-HARNESS §2).
//
// Two verified surfaces, nothing else:
//   - continue: `opencode run -s <id> --format json <text>`
//   - create:   `opencode run --title <t> --format json <text>`
//
// With adapters.opencode.server_url set, the run is attached to that server
// (`--attach <url>`, with the configured basic auth), and a zero exit is
// reported as state "accepted": the server owns the turn, this process only
// handed it over.
package opencode

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// Name is the stable harness id.
const Name = "opencode"

// Config is adapters.opencode (§4.1).
type Config struct {
	Binary       string
	Workdir      string
	ServerURL    string
	Username     string
	PasswordFile string
	Timeout      time.Duration
}

// Adapter delivers messages into opencode sessions.
type Adapter struct {
	cfg Config
	now func() time.Time
}

// New builds the adapter. An empty binary means "opencode" on PATH.
func New(cfg Config) *Adapter {
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = Name
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = harness.DefaultTimeout
	}
	return &Adapter{cfg: cfg, now: time.Now}
}

// Name is the harness id used in config, receipts and metrics.
func (a *Adapter) Name() string { return Name }

// Features reports what this adapter can do here. attach=true means a server
// URL is configured and deliveries are acknowledged, not just dispatched.
func (a *Adapter) Features() map[string]bool {
	return map[string]bool{
		"continue": true,
		"create":   true,
		"json":     true,
		"attach":   strings.TrimSpace(a.cfg.ServerURL) != "",
	}
}

// Probe measures the binary. It never starts a session.
func (a *Adapter) Probe(ctx context.Context) (harness.Capability, error) {
	return harness.ProbeBinary(ctx, a.cfg.Binary, a.Features())
}

// Deliver creates (empty sess.ID) or continues an opencode session.
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
	args, err := a.args(sess, msg)
	if err != nil {
		return harness.FailedReceipt(Name, harness.CodeUnavailable, err.Error()), nil
	}

	res := harness.RunCommand(ctx, harness.ExecSpec{Path: path, Args: args, Dir: workdir}, a.cfg.Timeout)
	created := sess.ID == ""
	state := harness.StateDispatched
	if a.serverMode() {
		// The server owns the turn; exit 0 means it acknowledged the handoff.
		state = harness.StateAccepted
	}
	rc := harness.Receipt{
		Harness:     Name,
		Created:     created,
		State:       state,
		ExitCode:    res.ExitCode,
		DurationMS:  time.Since(start).Milliseconds(),
		StdoutBytes: res.StdoutSize,
		At:          a.now().UTC(),
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
	}

	if sess.ID != "" {
		rc.SessionID = sess.ID
		return rc, nil
	}
	rc.SessionID = harness.ExtractSessionID(res.Stdout)
	if rc.SessionID == "" && len(res.Stdout) > 0 {
		// The turn happened (exit 0) so the state stays dispatched/accepted;
		// only the receipt is downgraded, exactly as §5 requires.
		rc.Error = harness.CodeOutputInvalid
		rc.Tail = firstLine(string(res.Stdout))
	}
	return rc, nil
}

// args builds the argument vector. The message text is the last element and is
// never touched: no quoting, no shell.
func (a *Adapter) args(sess harness.SessionRef, msg harness.Message) ([]string, error) {
	args := []string{"run"}
	if sess.ID != "" {
		args = append(args, "-s", sess.ID)
	} else {
		args = append(args, "--title", title(sess, msg))
	}
	args = append(args, "--format", "json")
	if url := strings.TrimSpace(a.cfg.ServerURL); url != "" {
		args = append(args, "--attach", url)
		if user := strings.ToLower(strings.TrimSpace(a.cfg.Username)); user != "" {
			args = append(args, "-u", user)
		} else {
			args = append(args, "-u", "opencode")
		}
		password, err := a.password()
		if err != nil {
			return nil, err
		}
		args = append(args, "-p", password)
	}
	args = append(args, sess.Args...)
	args = append(args, msg.Text)
	return args, nil
}

// serverMode reports whether the configured surface is a server ack.
func (a *Adapter) serverMode() bool { return strings.TrimSpace(a.cfg.ServerURL) != "" }

// password reads the attach password from password_file. A missing file is a
// configuration fault reported as unavailable, never an empty credential sent
// to the server.
func (a *Adapter) password() (string, error) {
	path := strings.TrimSpace(a.cfg.PasswordFile)
	if path == "" {
		return "", fmt.Errorf("adapters.opencode.server_url is set but password_file is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read password_file: %w", err)
	}
	pass := strings.TrimRight(string(raw), "\r\n")
	if pass == "" {
		return "", fmt.Errorf("password_file %s is empty", path)
	}
	return pass, nil
}

// title is the create-path session title: the courier session id, so an
// operator can correlate the harness listing with a courier session.
func title(sess harness.SessionRef, msg harness.Message) string {
	if msg.SessionID != "" {
		return "courier " + msg.SessionID
	}
	if msg.ID != "" {
		return "courier " + msg.ID
	}
	return "courier message"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
