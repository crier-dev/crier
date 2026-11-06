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
//
// # The provider knob (CRIER-HARNESS-13)
//
// codex resolves its provider from ~/.codex/config.toml, so the endpoint the
// adapter talks to depended on the host: a router that was down surfaced as
// "the CLI retries until the harness timeout", i.e. a bare HARNESS_TIMEOUT with
// no URL and no dial error. Config.BaseURL (env CR_CODEX_BASE_URL) is the
// adapter's own knob for that endpoint. It is passed to the CLI as a COMPLETE
// custom model provider, which is the only shape codex accepts: 0.166.0 refuses
// `-c model_providers.openai.base_url=…` outright ("model_providers contains
// reserved built-in provider IDs: `openai`"), so the override DEFINES the
// "crier" provider instead of editing the built-in one. The endpoint must speak
// the OpenAI Responses API (wire_api "responses"), which is codex's own
// protocol.
//
// On failure the adapter names what the CLI cannot: codex's retry line
// ("Reconnecting... waiting for network (Connection failed: error sending
// request)") carries neither the target nor the operating-system error, so
// when the endpoint refuses a TCP connection the receipt reports
// HARNESS_PROVIDER_UNREACHABLE and quotes the dial error AND the target URL.
// The diagnostic connection is made only on a failed delivery, is bounded by
// providerDialTimeout, and never runs on the success path.
package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// Name is the stable harness id.
const Name = "codex"

// DefaultSandbox is the mode §4.1 ships. It is applied when config leaves the
// field empty so the mode is never inherited from the user's config file.
const DefaultSandbox = "workspace-write"

// EnvBaseURL names the provider endpoint override Config.BaseURL also takes.
const EnvBaseURL = "CR_CODEX_BASE_URL"

// CodeProviderUnreachable is the receipt code for a delivery that failed
// because the provider endpoint the CLI is pointed at refused the connection.
// It is an adapter-level code in the §5 shape, like HARNESS_WORKDIR_MISSING and
// HARNESS_INTERRUPTED (§6): the catalog's HARNESS_EXIT_NONZERO cannot say WHY
// the turn died, and the retry loop codex enters against a dead endpoint
// otherwise reaches the caller as a context-free HARNESS_TIMEOUT.
const CodeProviderUnreachable = "HARNESS_PROVIDER_UNREACHABLE"

// The custom provider block. "openai" is a reserved built-in id (codex 0.166.0
// rejects an override of it), so the override ships its own id; env_key keeps
// reading the credential the host already exports rather than inventing one.
const (
	providerID      = "crier"
	providerName    = "Crier"
	providerWireAPI = "responses"
	providerEnvKey  = "OPENAI_API_KEY"
)

// providerDialTimeout bounds the diagnostic TCP connect a failed delivery makes
// to the endpoint. It is a connect, not a round trip: a reachable host answers
// it in milliseconds, a dead one is refused immediately, and an unroutable one
// costs this and no more.
const providerDialTimeout = 2 * time.Second

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
	// BaseURL is the OpenAI-compatible (Responses API) endpoint the CLI is
	// pointed at, e.g. http://127.0.0.1:8080/v1. Empty leaves the CLI on
	// whatever ~/.codex/config.toml names. New() falls back to EnvBaseURL.
	BaseURL string
	// Exec runs one codex invocation; nil uses harness.RunCommand, which is
	// the production path. The seam lets an embedding run the CLI through a
	// sandbox or a wrapper — or stand a stub in for the binary — while the
	// adapter keeps building the same argv and parsing the same JSONL. It is
	// not a bypass: whatever runs still receives the argv the adapter built
	// and its output is still parsed as codex's.
	Exec ExecFunc
}

// ExecFunc runs one codex invocation under a per-delivery deadline.
type ExecFunc func(ctx context.Context, spec harness.ExecSpec, timeout time.Duration) harness.ExecResult

// Adapter delivers messages into codex sessions.
type Adapter struct {
	cfg Config
	now func() time.Time
}

// New builds the adapter. An empty binary means "codex" on PATH; an empty
// BaseURL means EnvBaseURL, so a host can be routed without a config field.
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
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = strings.TrimSpace(os.Getenv(EnvBaseURL))
	}
	if cfg.Exec == nil {
		cfg.Exec = harness.RunCommand
	}
	return &Adapter{cfg: cfg, now: time.Now}
}

// Name is the harness id used in config and receipts, and the harness label
// value on crier_harness_dispatches_total, crier_harness_dispatch_errors_total
// and crier_harness_dispatch_duration_seconds (CRIER-OBS-1).
func (a *Adapter) Name() string { return Name }

// Features reports the surfaces this adapter uses: `exec` (create), `exec
// resume` (continue), `--json` (the thread id comes from its first event), the
// explicit sandbox mode, and the provider override when one is configured.
func (a *Adapter) Features() map[string]bool {
	return map[string]bool{
		"continue": true,
		"create":   true,
		"resume":   true,
		"json":     true,
		"sandbox":  strings.TrimSpace(a.cfg.Sandbox) != "",
		"model":    strings.TrimSpace(a.cfg.Model) != "",
		"stdin":    true,
		"base_url": strings.TrimSpace(a.cfg.BaseURL) != "",
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
	res := a.cfg.Exec(ctx, harness.ExecSpec{
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
	// finish stamps the duration on the way out, so a failure that needed the
	// diagnostic connect reports the whole attempt and not the part before it.
	finish := func(r harness.Receipt) (harness.Receipt, error) {
		r.DurationMS = time.Since(start).Milliseconds()
		return r, nil
	}
	switch {
	case res.StartError != "":
		return harness.FailedReceipt(Name, harness.CodeUnavailable, res.StartError), nil
	case res.TimedOut:
		rc.State = harness.StateFailed
		rc.Error = harness.CodeTimeout
		rc.Tail = "delivery exceeded " + a.cfg.Timeout.String()
		// A deadline against an endpoint that refuses TCP is not a slow turn:
		// codex retries "waiting for network" until it is killed. Name the
		// cause instead of handing back a context-free timeout.
		if named, ok := a.providerDown(res, true); ok {
			rc.Error = CodeProviderUnreachable
			rc.Tail = named
		} else if target := a.targetURL(res); target != "" {
			rc.Tail += " at provider " + target
		}
		return finish(rc)
	case res.ExitCode != 0:
		// The harness's own stderr tail is the evidence: a 401, a missing
		// rollout and a sandbox refusal all surface here, unedited.
		rc.State = harness.StateFailed
		rc.Error = harness.ClassifyFailure(string(res.Stdout), string(res.Stderr))
		rc.Tail = res.Tail
		if named, ok := a.providerDown(res, false); ok {
			rc.Error = CodeProviderUnreachable
			rc.Tail = named
		}
		return finish(rc)
	}

	if rc.Created && rc.SessionID == "" {
		// Delivered but unbound: the Crier session needs the id codex minted,
		// so the receipt downgrades to OUTPUT_INVALID exactly as §5 requires.
		rc.Error = harness.CodeOutputInvalid
		rc.Tail = firstLine(string(res.Stdout))
	}
	return finish(rc)
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
	if base := strings.TrimSpace(a.cfg.BaseURL); base != "" {
		// One complete provider definition: codex refuses a base_url override
		// of a built-in provider, so the adapter ships its own id.
		args = append(args,
			"-c", "model_provider="+quoteTOML(providerID),
			"-c", "model_providers."+providerID+".name="+quoteTOML(providerName),
			"-c", "model_providers."+providerID+".base_url="+quoteTOML(base),
			"-c", "model_providers."+providerID+".wire_api="+quoteTOML(providerWireAPI),
			"-c", "model_providers."+providerID+".env_key="+quoteTOML(providerEnvKey),
		)
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

// quoteTOML renders a value as the TOML string `-c key="value"` expects. Every
// value that reaches it — sandbox modes, provider ids, model names, base URLs —
// is a value config already validated, so this only encodes it.
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

// connectionFailureMarkers are the classes a harness prints when it could not
// reach its provider. They are matched against stdout AND stderr because codex
// reports the retry loop as JSON on stdout while other CLIs use stderr.
var connectionFailureMarkers = []string{
	"connection failed",
	"waiting for network",
	"error sending request",
	"connection refused",
	"connect: connection refused",
	"no such host",
	"network is unreachable",
	"connection reset",
	"tls handshake",
	"unexpected eof",
	"i/o timeout",
}

// providerDown names a delivery that failed because the provider endpoint is
// unreachable, or reports false when the evidence does not say that.
//
// onTimeout relaxes the trigger: a delivery that hit its deadline against an
// endpoint that refuses TCP is unreachable even when the CLI printed nothing
// before it was killed (codex retries in silence for whole minutes). An
// exit-non-zero delivery must show a connection class first, so an auth or
// sandbox failure never gets relabelled by a probe.
//
// The returned tail carries the target URL and the operating system's own dial
// error — the two facts the CLI's retry line omits.
func (a *Adapter) providerDown(res harness.ExecResult, onTimeout bool) (string, bool) {
	evidence := string(res.Stdout) + "\n" + string(res.Stderr)
	target := a.targetURL(res)
	if target == "" {
		return "", false
	}
	if !onTimeout && !connectionFailure(evidence) {
		return "", false
	}
	refused, err := endpointRefused(target)
	if !refused {
		return "", false
	}
	named := "codex provider unreachable at " + target + ": " + err.Error()
	if line := connectionLine(evidence); line != "" {
		named += " (codex: " + line + ")"
	}
	return named, true
}

// targetURL is the provider endpoint: the configured knob, else whichever URL
// the CLI itself named in its failure output.
func (a *Adapter) targetURL(res harness.ExecResult) string {
	if base := strings.TrimSpace(a.cfg.BaseURL); base != "" {
		return base
	}
	return firstURL(string(res.Stdout) + "\n" + string(res.Stderr))
}

// endpointRefused reports whether a TCP connect to the endpoint is refused. A
// target that is not a URL with a host says nothing, so it is not a failure.
func endpointRefused(target string) (bool, error) {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false, nil
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	conn, err := net.DialTimeout("tcp", addr, providerDialTimeout)
	if err != nil {
		return true, err
	}
	_ = conn.Close()
	return false, nil
}

// connectionFailure reports whether the captured output carries a
// connection-class error.
func connectionFailure(evidence string) bool {
	return connectionLine(evidence) != ""
}

// connectionLine is the first line that names a connection-class failure,
// trimmed for a receipt tail. codex's own words are the useful half; the URL
// and the errno are supplied by the adapter.
func connectionLine(evidence string) string {
	for _, line := range strings.Split(evidence, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		for _, marker := range connectionFailureMarkers {
			if strings.Contains(low, marker) {
				return clip(line, 300)
			}
		}
	}
	return ""
}

// firstURL pulls the first http(s) URL out of the captured output. JSONL wraps
// it in quotes and prose wraps it in parentheses, so the scan stops at the
// delimiters those shapes use.
func firstURL(evidence string) string {
	for _, scheme := range []string{"http://", "https://"} {
		for i := 0; ; i++ {
			j := strings.Index(evidence[i:], scheme)
			if j < 0 {
				break
			}
			start := i + j
			end := start
			for end < len(evidence) && !strings.ContainsRune(" 	\r\n\"'),;]}", rune(evidence[end])) {
				end++
			}
			candidate := strings.TrimRight(evidence[start:end], ".")
			if parsed, err := url.Parse(candidate); err == nil && parsed.Host != "" {
				return candidate
			}
			i = start
		}
	}
	return ""
}

// clip bounds a quoted line so one runaway child cannot bloat a receipt.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
