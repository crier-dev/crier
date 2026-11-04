// Package pi adapts the pi coding agent (CRIER-HARNESS §2).
//
// One verified surface, nothing else:
//   - continue: `pi -p --session-id <id> "<text>"`
//   - create:   the same call, with an id pi does not have yet
//
// `pi --help` documents `--session-id <id>` as "Use the current directory's
// existing session with the ID, creating it otherwise". Create-or-continue is
// therefore ONE surface, and an id pi has never seen is the create path — not
// an error. That is why this adapter never answers HARNESS_SESSION_UNKNOWN for
// a fresh id: it generates one, records the id it used on the receipt, and
// reports Created from what actually happened (pi's own "creating a new
// session" line is the evidence when a caller supplied the id).
//
// Two deviations from the bare argv form, both from §6:
//   - text over 64 KiB goes on stdin (pi reads its prompt from stdin when no
//     message positional is given), because ARG_MAX would otherwise cap it;
//   - text starting with "@" goes on stdin too, because pi expands @-prefixed
//     arguments as file references and a Crier message must arrive as text.
package pi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// Name is the stable harness id.
const Name = "pi"

// maxArgBytes is where the message stops travelling in the argument vector.
const maxArgBytes = 64 << 10

// createdMarker is pi's own announcement that the id did not exist yet:
//
//	Warning: No project session found with id '<id>'; creating a new session with that id.
//
// It is advisory: when it is absent the receipt simply keeps Created=false.
const createdMarker = "creating a new session"

// Config is adapters.pi (§4.1).
type Config struct {
	Binary  string
	Workdir string
	Model   string
	// SessionDir is passed as --session-dir; empty means pi's default.
	SessionDir string
	// Timeout bounds one delivery; zero takes the harness default.
	Timeout time.Duration
}

// Adapter delivers messages into pi sessions.
type Adapter struct {
	cfg Config
	now func() time.Time
}

// New builds the adapter. An empty binary means "pi" on PATH.
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

// Features reports the surfaces this adapter uses. "print" is `-p/--print`
// (one non-interactive turn, no TUI) and "session_id" is `--session-id`; both
// are required for a delivery to be possible at all, so both are constants.
func (a *Adapter) Features() map[string]bool {
	return map[string]bool{
		"continue":    true,
		"create":      true,
		"print":       true,
		"session_id":  true,
		"json":        true,
		"model":       strings.TrimSpace(a.cfg.Model) != "",
		"session_dir": strings.TrimSpace(a.cfg.SessionDir) != "",
	}
}

// Probe measures the binary and its version. It never starts a session.
func (a *Adapter) Probe(ctx context.Context) (harness.Capability, error) {
	return harness.ProbeBinary(ctx, a.cfg.Binary, a.Features())
}

// Deliver creates (empty sess.ID) or continues a pi session. Because pi's
// --session-id creates an absent session, both paths are the same argv; only
// the id differs.
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
	created := id == ""
	if created {
		id = newSessionID(msg)
	}

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
		Created:     created,
		State:       harness.StateDispatched,
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

	// The id we handed to --session-id is the id pi used. On the create path
	// the harness may echo the id it minted — prefer that — but a continue must
	// report the id it was told to continue: an echoed different id belongs to
	// some other session and must not overwrite the binding the Crier holds.
	if created {
		if echoed := harness.ExtractSessionID(res.Stdout); echoed != "" {
			rc.SessionID = echoed
		}
	}
	if !rc.Created && strings.Contains(strings.ToLower(string(res.Stderr)), createdMarker) {
		// pi created the session the caller named. The receipt says so: a
		// silent create reported as a continue would hide a split transcript.
		rc.Created = true
	}
	return rc, nil
}

// argv builds the argument vector and the optional stdin body. Message text is
// ONE argv element (after the documented `--` option terminator, so text that
// begins with "-" is not parsed as a flag) or one stdin body — never a shell.
func (a *Adapter) argv(sess harness.SessionRef, msg harness.Message, id string) ([]string, string) {
	args := []string{"-p", "--mode", "json", "--session-id", id}
	if dir := strings.TrimSpace(a.cfg.SessionDir); dir != "" {
		args = append(args, "--session-dir", dir)
	}
	if model := a.model(sess); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, sess.Args...)
	if needsStdin(msg.Text) {
		return args, msg.Text
	}
	return append(args, "--", msg.Text), ""
}

// model is the per-session model override, else the configured one.
func (a *Adapter) model(sess harness.SessionRef) string {
	if m := strings.TrimSpace(sess.Model); m != "" {
		return m
	}
	return strings.TrimSpace(a.cfg.Model)
}

// needsStdin reports whether the text must bypass the argument vector: ARG_MAX
// for very long messages (§6), and pi's @file expansion for text that starts
// with "@" (§2 has no file-inclusion surface in this row).
func needsStdin(text string) bool {
	return len(text) > maxArgBytes || strings.HasPrefix(text, "@")
}

// newSessionID builds the create-path id. It is derived from the Crier
// session (else message) id so an operator can correlate pi's session listing
// with Crier, and falls back to random bytes when nothing usable remains.
// The alphabet is pi's file-safe subset: pi names the session file
// "<timestamp>_<id>.jsonl".
func newSessionID(msg harness.Message) string {
	base := strings.TrimSpace(msg.SessionID)
	if base == "" {
		base = strings.TrimSpace(msg.ID)
	}
	base = sanitizeID(base)
	if base == "" {
		var raw [4]byte
		if _, err := rand.Read(raw[:]); err == nil {
			base = hex.EncodeToString(raw[:])
		} else {
			base = fmt.Sprintf("%d", time.Now().UnixNano())
		}
	}
	const maxBase = 48
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	return "crier-" + base
}

// sanitizeID keeps [A-Za-z0-9._-] and turns everything else into a single "-",
// so a Crier id can never escape the session directory or split an argv.
func sanitizeID(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		case r == '-':
			if !dash {
				b.WriteRune('-')
				dash = true
			}
		default:
			if !dash {
				b.WriteRune('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}
