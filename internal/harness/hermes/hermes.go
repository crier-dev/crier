// Package hermes adapts the Hermes agent harness (CRIER-HARNESS §2).
//
// Two verified surfaces, nothing else:
//   - continue: `hermes chat -Q --resume <id> -q <text>`
//   - create:   gateway `POST /v1/responses` with no resume id
//
// The CLI cannot create a session through a verified surface, so when no
// gateway URL is configured the adapter reports create=false in its
// Capability.Features and a create delivery fails HARNESS_UNAVAILABLE rather
// than inventing a flag.
package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// Name is the stable harness id.
const Name = "hermes"

// Config is adapters.hermes (§4.1) plus the gateway the create path needs.
type Config struct {
	Binary       string
	Workdir      string
	Model        string
	GatewayURL   string
	GatewayToken string
	// Timeout bounds one delivery; zero takes the harness default.
	Timeout time.Duration
}

// Adapter delivers messages into Hermes sessions.
type Adapter struct {
	cfg    Config
	client *http.Client
	now    func() time.Time
}

// New builds the adapter. An empty binary means "hermes" on PATH.
func New(cfg Config) *Adapter {
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = Name
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = harness.DefaultTimeout
	}
	return &Adapter{
		cfg:    cfg,
		client: &http.Client{},
		now:    time.Now,
	}
}

// Name is the harness id used in config, receipts and metrics.
func (a *Adapter) Name() string { return Name }

// Features reports the two paths this adapter actually has.
func (a *Adapter) Features() map[string]bool {
	gateway := a.gatewayURL() != ""
	return map[string]bool{
		"continue": true,
		"create":   gateway,
		"gateway":  gateway,
		"cli":      true,
		"json":     false,
	}
}

// Probe measures the binary and reports which paths are configured. It never
// starts a session and never pre-flights credentials (§6): a harness that is
// installed but logged out is available, and Deliver carries its stderr tail.
func (a *Adapter) Probe(ctx context.Context) (harness.Capability, error) {
	cap, err := harness.ProbeBinary(ctx, a.cfg.Binary, a.Features())
	if err != nil {
		return cap, err
	}
	return cap, nil
}

// Deliver continues the named session, or creates one through the gateway when
// no id is set.
func (a *Adapter) Deliver(ctx context.Context, sess harness.SessionRef, msg harness.Message) (harness.Receipt, error) {
	start := a.now()
	workdir := sess.Workdir
	if workdir == "" {
		workdir = a.cfg.Workdir
	}
	if err := harness.WorkdirError(workdir); err != nil {
		return harness.FailedReceipt(Name, harness.CodeWorkdirMissing, "harness workdir "+workdir+": "+err.Error()), nil
	}
	if sess.ID == "" {
		return a.create(ctx, sess, msg, start)
	}
	return a.continueCLI(ctx, sess, msg, workdir, start)
}

// continueCLI runs `hermes chat -Q --resume <id> -q <text>`.
func (a *Adapter) continueCLI(ctx context.Context, sess harness.SessionRef, msg harness.Message, workdir string, start time.Time) (harness.Receipt, error) {
	path, err := harness.LookupBinary(a.cfg.Binary)
	if err != nil {
		return harness.FailedReceipt(Name, harness.CodeUnavailable, "binary not found: "+a.cfg.Binary), nil
	}
	args := []string{"chat", "-Q", "--resume", sess.ID}
	args = append(args, sess.Args...)
	args = append(args, "-q", msg.Text)

	res := harness.RunCommand(ctx, harness.ExecSpec{Path: path, Args: args, Dir: workdir}, a.cfg.Timeout)
	rc := harness.Receipt{
		Harness:     Name,
		SessionID:   sess.ID,
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
	return rc, nil
}

// gatewayRequest is the create-path body. Only fields the surface documents are
// sent; message text is a JSON value, never a command line.
type gatewayRequest struct {
	Input string `json:"input"`
	Model string `json:"model,omitempty"`
}

// create calls the Hermes gateway: POST /v1/responses with no resume id.
func (a *Adapter) create(ctx context.Context, sess harness.SessionRef, msg harness.Message, start time.Time) (harness.Receipt, error) {
	base := a.gatewayURL()
	if base == "" {
		return harness.FailedReceipt(Name, harness.CodeUnavailable,
			"creating a Hermes session needs adapters.hermes.gateway_url; the verified CLI path only continues an existing session"), nil
	}
	model := sess.Model
	if model == "" {
		model = a.cfg.Model
	}
	body, err := json.Marshal(gatewayRequest{Input: msg.Text, Model: model})
	if err != nil {
		return harness.FailedReceipt(Name, harness.CodeUnavailable, "encode gateway request: "+err.Error()), nil
	}
	url := strings.TrimRight(base, "/") + "/v1/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return harness.FailedReceipt(Name, harness.CodeUnavailable, "build gateway request: "+err.Error()), nil
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.GatewayToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.GatewayToken)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return harness.FailedReceipt(Name, harness.CodeUnavailable, "gateway unreachable: "+err.Error()), nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	rc := harness.Receipt{
		Harness:     Name,
		Created:     true,
		State:       harness.StateAccepted,
		DurationMS:  time.Since(start).Milliseconds(),
		StdoutBytes: len(raw),
		At:          a.now().UTC(),
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// There is no process behind this path, so ExitCode carries the HTTP
		// status as the closest evidence of what the gateway answered.
		rc.State = harness.StateFailed
		rc.ExitCode = resp.StatusCode
		rc.Error = harness.CodeExitNonZero
		rc.Tail = tail(raw)
		return rc, nil
	}
	rc.SessionID = harness.ExtractSessionID(raw)
	if rc.SessionID == "" {
		// Delivered, but the id the Crier session needs is missing: the
		// receipt says so instead of implying a binding it does not have.
		rc.Error = harness.CodeOutputInvalid
	}
	return rc, nil
}

func (a *Adapter) gatewayURL() string { return strings.TrimSpace(a.cfg.GatewayURL) }

// tail keeps the last 2 KiB of a response body for the receipt.
func tail(b []byte) string {
	const n = 2048
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}
