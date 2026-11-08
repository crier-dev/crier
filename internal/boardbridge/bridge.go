// Package boardbridge delivers versioned, allowlisted command envelopes to boardctl.
package boardbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const DefaultAllowedOps = "show,list,next-id,create,update,event"

var validOps = map[string]bool{"show": true, "list": true, "next-id": true, "create": true, "update": true, "event": true}

// Config is the per-agent boardctl transport configuration.
type Config struct {
	BoardDir        string   `json:"board_dir"`
	BinPath         string   `json:"bin_path,omitempty"`
	AllowedOps      string   `json:"allowed_ops,omitempty"`
	TimeoutSecs     int      `json:"timeout_secs,omitempty"`
	SenderAllowlist []string `json:"sender_allowlist,omitempty"`
}

func (c *Config) Validate() error {
	if c == nil {
		return errors.New("boardctl config is nil")
	}
	if strings.TrimSpace(c.BoardDir) == "" {
		return errors.New("boardctl.board_dir is required")
	}
	if c.TimeoutSecs != 0 && (c.TimeoutSecs < 5 || c.TimeoutSecs > 120) {
		return errors.New("boardctl.timeout_secs must be 5..120")
	}
	for _, op := range c.ops() {
		if !validOps[op] {
			return fmt.Errorf("boardctl.allowed_ops: unknown op %q", op)
		}
	}
	for _, sender := range c.SenderAllowlist {
		if strings.TrimSpace(sender) == "" {
			return errors.New("boardctl.sender_allowlist entries must not be empty")
		}
	}
	return nil
}
func (c *Config) ops() []string {
	s := c.AllowedOps
	if s == "" {
		s = DefaultAllowedOps
	}
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}
func (c *Config) timeout() time.Duration {
	if c.TimeoutSecs == 0 {
		return 30 * time.Second
	}
	return time.Duration(c.TimeoutSecs) * time.Second
}
func (c *Config) AllowsSender(sender string) bool {
	for _, v := range c.SenderAllowlist {
		if sender == v {
			return true
		}
	}
	return false
}

// Error is a structured transport failure suitable for the delivery reply.
type Error struct {
	ErrorKind    string `json:"error_kind"`
	ExitCode     int    `json:"exit_code"`
	BoardctlText string `json:"boardctl_text"`
}

func (e *Error) Error() string { return e.ErrorKind + ": " + e.BoardctlText }

type Envelope struct {
	Version int            `json:"v"`
	Op      string         `json:"op"`
	Args    map[string]any `json:"args"`
}
type Result struct {
	Payload json.RawMessage `json:"payload"`
	Text    string          `json:"text,omitempty"`
}

// Bridge serializes invocations per agent and never interprets message text as argv.
type Bridge struct {
	bin        string
	startupErr string
	mu         sync.Mutex
}

const maxReplyBytes = 64 * 1024

func New(c *Config, globalBin string) (*Bridge, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	bin := c.BinPath
	if bin == "" {
		bin = globalBin
	}
	if bin == "" {
		bin = "boardctl"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("CR_BOARDCTL_BIN %q: %w", bin, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil || st.Mode()&0111 == 0 {
		return nil, fmt.Errorf("CR_BOARDCTL_BIN %q is missing or not executable", abs)
	}
	bridge := &Bridge{bin: abs}
	bridge.startupProbe(c.BoardDir)
	return bridge, nil
}

func (b *Bridge) startupProbe(boardDir string) {
	doctor := exec.Command(b.bin, "-C", boardDir, "doctor")
	if err := doctor.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		slog.Error("boardctl doctor exit=" + fmt.Sprint(code))
	}
	version := exec.Command(b.bin, "version", "--json")
	_ = version.Run()
	header := exec.Command(b.bin, "-C", boardDir, "header", "--json")
	out, err := header.Output()
	if err != nil || !json.Valid(bytes.TrimSpace(out)) {
		b.startupErr = fmt.Sprintf("boardctl refused: board header unparsable at %s", boardDir)
	}
}

func (b *Bridge) Deliver(ctx context.Context, c *Config, sender string, raw []byte) (*Result, error) {
	if b.startupErr != "" {
		return nil, &Error{ErrorKind: "refused", ExitCode: -1, BoardctlText: b.startupErr}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, &Error{ErrorKind: "refused", ExitCode: -1, BoardctlText: "empty command"}
	}
	if !c.AllowsSender(sender) {
		return nil, errors.New("sender not allowlisted")
	}
	env, err := Parse(c, raw)
	if err != nil {
		return nil, err
	}
	argv, err := Arguments(c, env)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st, statErr := os.Stat(b.bin)
	if statErr != nil || st.Mode()&0111 == 0 {
		return nil, &Error{ErrorKind: "unavailable", ExitCode: -1, BoardctlText: "pinned binary " + b.bin + " no longer executable"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	cmd := exec.Command(b.bin, argv...)
	cmd.Dir = c.BoardDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, &Error{ErrorKind: "unavailable", ExitCode: -1, BoardctlText: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	for !finished {
		select {
		case err = <-done:
			finished = true
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-done
			finished = true
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, &Error{ErrorKind: "timeout", ExitCode: -1, BoardctlText: fmt.Sprintf("timeout after %ds, process group killed", int(c.timeout().Seconds()))}
			}
			return nil, ctx.Err()
		}
	}
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return nil, &Error{ErrorKind: "refused", ExitCode: code, BoardctlText: stderr.String()}
	}
	if env.Op == "create" || env.Op == "update" || env.Op == "event" {
		auditArgs := []string{"-C", c.BoardDir, "event", "--type", "audit", "--actor", "board-manager", "--detail-text", env.Op + " " + fmt.Sprint(env.Args), "--json"}
		audit := exec.CommandContext(ctx, b.bin, auditArgs...)
		audit.Dir = c.BoardDir
		var auditErr bytes.Buffer
		audit.Stderr = &auditErr
		if err := audit.Run(); err != nil {
			return nil, &Error{ErrorKind: "crash", ExitCode: -1, BoardctlText: auditErr.String()}
		}
	}
	payload := bytes.TrimSpace(stdout.Bytes())
	if !json.Valid(payload) {
		return nil, &Error{ErrorKind: "crash", ExitCode: 0, BoardctlText: "boardctl returned invalid JSON"}
	}
	text := replySummary(env, payload)
	if env.Op == "create" || env.Op == "update" {
		text += " … (uncommitted; board files commit via your board flow)"
	}
	if len(payload) > maxReplyBytes {
		truncated := len(payload) - (maxReplyBytes - 256)
		note := fmt.Sprintf("… (%d bytes truncated; use a narrower filter)", truncated)
		encoded, _ := json.Marshal(string(payload[:maxReplyBytes-256]) + note)
		payload = encoded
	}
	return &Result{Payload: append(json.RawMessage(nil), payload...), Text: text}, nil
}

func replySummary(env Envelope, payload []byte) string {
	var value any
	_ = json.Unmarshal(payload, &value)
	if env.Op == "list" {
		count := 0
		switch v := value.(type) {
		case []any:
			count = len(v)
		case map[string]any:
			if rows, ok := v["rows"].([]any); ok {
				count = len(rows)
			} else {
				count = len(v)
			}
		}
		return fmt.Sprintf("Listed %d rows", count)
	}
	if env.Op == "create" || env.Op == "update" {
		id, _ := env.Args["id"].(string)
		status := ""
		if requested, ok := env.Args["status"].(string); ok {
			status = requested
		}
		if row, ok := value.(map[string]any); ok {
			if v, ok := row["id"].(string); ok && v != "" {
				id = v
			}
			if v, ok := row["status"].(string); ok && v != "" {
				status = v
			}
			if nested, ok := row["row"].(map[string]any); ok {
				if v, ok := nested["status"].(string); ok {
					status = v
				}
			}
		}
		return fmt.Sprintf("Row %s status %s", strings.Join(strings.Fields(id), " "), strings.Join(strings.Fields(status), " "))
	}
	return "Boardctl command completed"
}

func Parse(c *Config, raw []byte) (Envelope, error) {
	var e Envelope
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, fmt.Errorf("refused: invalid command envelope: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return e, errors.New("refused: trailing data in command envelope")
	}
	if e.Version != 1 {
		return e, fmt.Errorf("refused: unsupported envelope version %d", e.Version)
	}
	allowed := false
	for _, op := range c.ops() {
		if op == e.Op {
			allowed = true
		}
	}
	if !allowed {
		return e, fmt.Errorf("refused: op %q not allowed for this agent", e.Op)
	}
	if e.Args == nil {
		e.Args = map[string]any{}
	}
	if err := validateArgs(e.Op, e.Args); err != nil {
		return e, err
	}
	return e, nil
}
func schema(op string) map[string]string {
	switch op {
	case "show":
		return map[string]string{"id": "string"}
	case "list":
		return map[string]string{"status": "string", "family": "string", "limit": "number"}
	case "next-id":
		return map[string]string{"family": "string"}
	case "create":
		return map[string]string{"id": "string", "title": "string", "priority": "string", "complexity": "string", "depends_on": "strings", "reasoning": "string", "capability_tags": "strings", "acceptance_criteria": "strings"}
	case "update":
		return map[string]string{"id": "string", "status": "string", "evidence_run_id": "string", "reason": "string", "title": "string", "priority": "string", "complexity": "string", "depends_on": "strings", "acceptance_criteria": "strings"}
	case "event":
		return map[string]string{"type": "string", "detail_text": "string", "actor": "string"}
	}
	return nil
}
func validateArgs(op string, a map[string]any) error {
	required := map[string][]string{"show": {"id"}, "next-id": {"family"}, "create": {"id", "title"}, "update": {"id"}, "event": {"type", "detail_text"}}
	for _, key := range required[op] {
		if _, ok := a[key]; !ok {
			return fmt.Errorf("refused: args.%s is required for op %q", key, op)
		}
	}
	s := schema(op)
	for k, v := range a {
		typ, ok := s[k]
		if !ok {
			return fmt.Errorf("refused: args key %q is not allowed for op %q", k, op)
		}
		switch typ {
		case "string":
			x, ok := v.(string)
			if !ok || x == "" || strings.HasPrefix(x, "-") {
				return fmt.Errorf("refused: args.%s must be a non-empty string not beginning with '-'", k)
			}
		case "number":
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("refused: args.%s must be a number", k)
			}
		case "strings":
			x, ok := v.([]any)
			if !ok {
				return fmt.Errorf("refused: args.%s must be an array of strings", k)
			}
			for _, item := range x {
				z, ok := item.(string)
				if !ok || strings.HasPrefix(z, "-") {
					return fmt.Errorf("refused: args.%s values must be strings not beginning with '-'", k)
				}
			}
		}
	}
	return nil
}
func Arguments(c *Config, e Envelope) ([]string, error) {
	if err := validateArgs(e.Op, e.Args); err != nil {
		return nil, err
	}
	args := []string{"-C", c.BoardDir, e.Op}
	add := func(flag string, v any) {
		args = append(args, "--"+flag)
		if s, ok := v.(string); ok {
			args = append(args, s)
			return
		}
		if a, ok := v.([]any); ok {
			for _, x := range a {
				args = append(args, x.(string))
			}
			return
		}
		args = append(args, fmt.Sprint(v))
	}
	if e.Op == "show" {
		args = append(args, e.Args["id"].(string), "--json")
		return args, nil
	}
	if e.Op == "update" {
		if id, ok := e.Args["id"].(string); ok {
			args = append(args, id)
		}
	}
	order := map[string][]string{"list": {"status", "family", "limit"}, "next-id": {"family"}, "create": {"id", "title", "priority", "complexity", "depends_on", "reasoning", "capability_tags", "acceptance_criteria"}, "update": {"status", "evidence_run_id", "reason", "title", "priority", "complexity", "depends_on", "acceptance_criteria"}, "event": {"type", "detail_text", "actor"}}
	for _, k := range order[e.Op] {
		if v, ok := e.Args[k]; ok {
			flag := k
			if k == "evidence_run_id" {
				flag = "evidence-run-id"
			}
			if k == "detail_text" {
				flag = "detail-text"
			}
			if k == "capability_tags" {
				flag = "capability-tags"
			}
			if k == "acceptance_criteria" {
				flag = "acceptance-criteria"
			}
			if k == "depends_on" {
				flag = "depends-on"
			}
			add(flag, v)
		}
	}
	args = append(args, "--json")
	return args, nil
}
