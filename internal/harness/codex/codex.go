// Package codex adapts the OpenAI codex CLI (CRIER-HARNESS §2).
//
// Two verified surfaces, nothing else:
//   - create:   `codex exec "<prompt>"` (a new session)
//   - continue: `codex exec resume <id> "<prompt>"`
//
// Unlike pi, codex does not create-or-continue: `codex exec resume <missing>`
// fails ("no rollout found for thread id …"). The adapter reports that as
// HARNESS_SESSION_UNKNOWN and never retries without the id — a silent create
// would fork the transcript (§4.3).
//
// The sandbox mode is always passed explicitly, from config: `-s <mode>` on the
// create path and `-c sandbox_mode="<mode>"` on resume (resume does not accept
// -s). Neither path inherits the mode from ~/.codex/config.toml, which is what
// makes a delivery reproducible on a host whose user config differs.
//
// Text over 64 KiB goes on stdin via the documented `-` prompt (§6).
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// Name is the stable harness id.
const Name = "codex"

// DefaultSandbox is the mode §4.1 ships. It is applied when config leaves the
// field empty so the mode is never inherited from the user's config file.
const DefaultSandbox = "workspace-write"

// maxArgBytes is where the message stops travelling in the argument vector.
const maxArgBytes = 64 << 10

// threadMarker is the JSONL event codex prints first with `--json`; its
// thread_id is the value `codex exec resume` takes.
const threadMarker = "thread.started"

// Config is adapters.codex (§4.1).
type Config struct {
	Binary  string
	Workdir string
	Model   string
	// Sandbox is the codex sandbox mode passed to exec; empty means
	// DefaultSandbox.
	Sandbox string
	// Timeout bounds one delivery; zero takes the harness default.
	Timeout time.Duration
}

// Adapter delivers messages into codex sessions.
type Adapter struct {
	cfg Config
	now func() time.Time
}

// New builds the adapter. An empty binary means "codex" on PATH.
func New(cfg Config) *Adapter {
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = Name
	}
	if strings.TrimSpace(cfg.Sandbox) == "" {
		cfg.Sandbox = DefaultSandbox
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

// Features reports the surfaces this adapter uses: `exec` (create), `exec
// resume` (continue), `--json` (the thread id comes from its first event) and
// the explicit sandbox mode.
func (a *Adapter) Features() map[string]bool {
	return map[string]bool{
		"continue": true,
		"create":   true,
		"resume":   true,
		"json":     true,
		"sandbox":  strings.TrimSpace(a.cfg.Sandbox) != "",
		"model":    strings.TrimSpace(a.cfg.Model) != "",
		"stdin":    true,
	}
}

// Probe measures the binary and its version. It never starts a session and
// never pre-flights credentials: a logged-out CLI is available, and Deliver
// carries its own stderr tail (§6).
func (a *Adapter) Probe(ctx context.Context) (harness.Capability, error) {
	return harness.ProbeBinary(ctx, a.cfg.Binary, a.Features())
}

// Deliver creates (empty sess.ID) or continues a codex session.
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
	if id == "" {
		// codex mints the thread id itself, and it is on stdout as soon as the
		// thread exists — before the turn completes. Take it even from a failed
		// run: a thread that was created and then timed out is resumable, and
		// the receipt is the only place the Crier can learn its id.
		if tid := ThreadID(res.Stdout); tid != "" {
			rc.SessionID = tid
		}
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
		// The harness's own stderr tail is the evidence: a 401, a missing
		// rollout and a sandbox refusal all surface here, unedited.
		rc.State = harness.StateFailed
		rc.Error = harness.ClassifyFailure(string(res.Stdout), string(res.Stderr))
		rc.Tail = res.Tail
		return rc, nil
	}

	if rc.Created && rc.SessionID == "" {
		// Delivered but unbound: the Crier session needs the id codex minted,
		// so the receipt downgrades to OUTPUT_INVALID exactly as §5 requires.
		rc.Error = harness.CodeOutputInvalid
		rc.Tail = firstLine(string(res.Stdout))
	}
	return rc, nil
}

// argv builds the argument vector and the optional stdin body. Message text is
// one positional after the `--` option terminator, or one stdin body.
func (a *Adapter) argv(sess harness.SessionRef, msg harness.Message, id string) ([]string, string) {
	create := id == ""
	args := []string{"exec"}
	if !create {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	if create {
		args = append(args, "-s", a.cfg.Sandbox)
	} else {
		// `codex exec resume --help` has no -s; the config override is the
		// documented way to pin the same mode on a resumed thread.
		args = append(args, "-c", "sandbox_mode="+quoteTOML(a.cfg.Sandbox))
	}
	if model := a.model(sess); model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, sess.Args...)
	if !create {
		args = append(args, id)
	}
	if len(msg.Text) > maxArgBytes {
		// `codex exec -` / `codex exec resume <id> -` read the prompt from
		// stdin (documented in `codex exec --help`).
		return append(args, "-"), msg.Text
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

// quoteTOML renders a sandbox mode as the TOML string `-c key="value"` expects.
// Modes are a closed set (read-only, workspace-write, danger-full-access), so
// this only encodes a value config already validated by codex itself.
func quoteTOML(v string) string {
	if !strings.ContainsAny(v, "\"\\") {
		return `"` + v + `"`
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return `"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return string(encoded)
}

// ThreadID pulls the codex thread id out of `--json` output. The first event of
// a run is {"type":"thread.started","thread_id":"<uuid>"}; older or wrapped
// shapes are covered by the shared session-id reader.
func ThreadID(stdout []byte) string {
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		if id := rawString(obj, "thread_id"); id != "" {
			return id
		}
		if strings.Contains(line, threadMarker) {
			// The event is the thread handshake; an id-less one is a broken
			// line, not a session.
			continue
		}
	}
	return harness.ExtractSessionID(stdout)
}

func rawString(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
