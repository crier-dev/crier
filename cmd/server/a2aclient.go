package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/a2a/client"
)

// INT-A2A-007. `crier a2a-client` is the SHIPPED surface of crier's OUTBOUND A2A
// client (specs/A2A-OPTION.md §1: "lets a crier agent be an A2A client").
//
// It is a subcommand for the same reason `crier keygen` is one (CR-FEAT-027):
// the capability is useful only if an operator can reach it without a test
// harness, and it must never start, reconfigure or disturb the server. The
// subcommand dispatches BEFORE parseArgs, so `crier`, `crier -port 9001` and
// every other argument shape keep the pre-existing behaviour exactly (the
// non-regression contract of §6).
//
// It requires NOTHING of the local relay: CR_A2A_ENABLED gates crier's own A2A
// SERVER routes (§4.1), and this is an outbound-only surface. A relay with its
// A2A server switched off — the live fleet bus — can address a foreign agent
// with it, which is the posture this row was built in.
//
// WHAT IT RECORDS. Every verb that succeeds writes a record: the peer task id
// (or the message id when the peer answered with a direct Message) together
// with the digest of the exact card bytes it addressed. With `-record PATH` that
// record is a JSON file the operator can read afterwards (mode 0600, so it
// carries no more than it must); with `-json` it is printed on stdout as well.
// The digest is the publisher's own convention — the same SHA-256 an Agent
// Card's ETag is derived from (§5.3.2) — so a recorded digest can be compared
// against the peer's served validator directly.

// The exit codes are a CONTRACT: each named failure has its own, so a script
// can branch on the failure class without parsing prose. They are also printed
// in the JSON error object, and the mapping is asserted in
// a2aclient_test.go.
const (
	a2aExitOK                 = 0
	a2aExitUnreachable        = 1
	a2aExitUsage              = 2
	a2aExitCardNotFound       = 3
	a2aExitInvalidResponse    = 4
	a2aExitUnauthenticated    = 5
	a2aExitCardMalformed      = 6
	a2aExitCredentialRequired = 7
	a2aExitPeerRefused        = 8
	a2aExitPeerHTTPError      = 9
	a2aExitCredentialInvalid  = 10
	a2aExitConfigInvalid      = 11
)

// a2aExitCode maps a client failure onto its exit code by NAME, so the contract
// is visible in one place.
func a2aExitCode(err error) int {
	switch client.ErrorName(err) {
	case client.NamePeerUnreachable:
		return a2aExitUnreachable
	case client.NameCardNotFound:
		return a2aExitCardNotFound
	case client.NameInvalidResponse:
		return a2aExitInvalidResponse
	case client.NameUnauthenticated:
		return a2aExitUnauthenticated
	case client.NameCardMalformed:
		return a2aExitCardMalformed
	case client.NameCredentialRequired:
		return a2aExitCredentialRequired
	case client.NamePeerRefused:
		return a2aExitPeerRefused
	case client.NamePeerHTTPError:
		return a2aExitPeerHTTPError
	case client.NameCredentialInvalid:
		return a2aExitCredentialInvalid
	case client.NameConfigInvalid:
		return a2aExitConfigInvalid
	default:
		return a2aExitUnreachable
	}
}

// a2aClientRecord is the artifact a verb leaves behind: WHICH peer and card it
// addressed, and the peer's own identifier for the work.
type a2aClientRecord struct {
	Operation   string `json:"operation"`
	Origin      string `json:"origin"`
	AgentID     string `json:"agent_id,omitempty"`
	Tenant      string `json:"tenant,omitempty"`
	Endpoint    string `json:"endpoint"`
	Wire        string `json:"wire"`
	CardURL     string `json:"card_url"`
	CardName    string `json:"card_name"`
	CardVersion string `json:"card_version,omitempty"`
	CardDigest  string `json:"card_digest"`
	CardETag    string `json:"card_etag,omitempty"`
	MessageID   string `json:"message_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	State       string `json:"state,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	Replayed    bool   `json:"replayed"`
	At          string `json:"at"`
}

// a2aClientError is the machine-readable failure shape `-json` prints.
type a2aClientError struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	Status    int    `json:"status,omitempty"`
	ExitCode  int    `json:"exit_code"`
}

// a2aClientFlags is the whole flag surface. One set for every verb keeps the
// help text honest; a flag that a verb does not use is REFUSED when it is set
// (see checkVerbFlags) rather than silently ignored, which is crier's rule for
// every request-level parameter (DF-CRIER-180).
type a2aClientFlags struct {
	url      string
	agentID  string
	scheme   string
	credFile string
	credEnv  string
	wire     string
	record   string
	json     bool
	timeout  time.Duration

	// send
	text      string
	messageID string
	contextID string

	// get / cancel
	task string
}

// runA2AClient implements `crier a2a-client <verb> [flags]`. It returns a
// process exit code and writes everything it has to say to out/errOut, so the
// whole surface is assertable from a test without touching the process streams.
func runA2AClient(args []string, out, errOut io.Writer, getenv config.Getenv) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printA2AClientUsage(out)
		if len(args) == 0 {
			return a2aExitUsage
		}
		return a2aExitOK
	}
	verb := args[0]
	switch verb {
	case "card", "send", "get", "cancel":
	default:
		_, _ = fmt.Fprintf(errOut, "crier a2a-client: unknown verb %q (want card, send, get or cancel)\n", verb)
		printA2AClientUsage(errOut)
		return a2aExitUsage
	}

	var f a2aClientFlags
	fs := flag.NewFlagSet("crier a2a-client "+verb, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&f.url, "url", "", "peer origin, e.g. https://peer.example (required)")
	fs.StringVar(&f.agentID, "agent-id", "", "agent_id discovery selector (a crier relay serves many agents behind one origin)")
	fs.StringVar(&f.scheme, "scheme", "bearerAuth", "the peer card's security-scheme name this credential satisfies")
	fs.StringVar(&f.credFile, "credential-file", "", "file holding the credential (mode 0600; never a command-line value)")
	fs.StringVar(&f.credEnv, "credential-env", "", "environment variable holding the credential")
	fs.StringVar(&f.wire, "wire", "v1", "method-name dialect: v1 (SendMessage/GetTask/CancelTask, A2A v1.0.0) or legacy (message/send)")
	fs.StringVar(&f.record, "record", "", "write the record (peer task id + card digest) to this JSON file")
	fs.BoolVar(&f.json, "json", false, "print the result (or the failure) as a single JSON object")
	fs.DurationVar(&f.timeout, "timeout", client.DefaultTimeout, "per-request timeout")
	fs.StringVar(&f.text, "text", "", "send: the message text (one text part)")
	fs.StringVar(&f.messageID, "message-id", "", "send: pin the idempotency key (a retry with the same value returns the SAME task)")
	fs.StringVar(&f.contextID, "context-id", "", "send: the conversation this message belongs to")
	fs.StringVar(&f.task, "task", "", "get/cancel: the task id")
	fs.Usage = func() { printA2AClientUsage(errOut) }
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return a2aExitOK
		}
		return a2aExitUsage
	}
	if err := checkVerbFlags(verb, fs, &f); err != nil {
		_, _ = fmt.Fprintf(errOut, "crier a2a-client %s: %v\n", verb, err)
		return a2aExitUsage
	}
	if len(fs.Args()) > 0 {
		_, _ = fmt.Fprintf(errOut, "crier a2a-client %s: unexpected argument %q\n", verb, fs.Args()[0])
		return a2aExitUsage
	}

	c, err := buildA2AClient(&f, getenv)
	if err != nil {
		return a2aClientFailure(out, errOut, verb, err, f.json)
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.timeout*2)
	defer cancel()

	card, err := c.Discover(ctx)
	if err != nil {
		return a2aClientFailure(out, errOut, verb, err, f.json)
	}
	record := a2aClientRecord{
		Operation:   verb,
		Origin:      c.Origin(),
		AgentID:     f.agentID,
		Tenant:      card.Tenant,
		Endpoint:    card.Endpoint,
		Wire:        c.Methods().Name,
		CardURL:     card.URL,
		CardName:    card.AgentCard.Name,
		CardVersion: card.AgentCard.Version,
		CardDigest:  card.Digest,
		CardETag:    card.CardETag,
		At:          time.Now().UTC().Format(time.RFC3339),
	}

	switch verb {
	case "card":
		// Nothing to send: the card itself is the result.
	case "send":
		res, sendErr := c.Send(ctx, card, client.SendRequest{
			MessageID: f.messageID,
			ContextID: f.contextID,
			Text:      f.text,
		})
		if sendErr != nil {
			return a2aClientFailure(out, errOut, verb, sendErr, f.json)
		}
		record.MessageID = res.MessageID
		record.TaskID = res.TaskID
		record.Attempts = res.Attempts
		record.Replayed = res.Replayed
		record.State = taskState(res.Task, res.Message)
	case "get":
		res, getErr := c.GetTask(ctx, card, f.task, nil)
		if getErr != nil {
			return a2aClientFailure(out, errOut, verb, getErr, f.json)
		}
		record.TaskID = res.TaskID
		record.State = string(res.Task.Status.State)
	case "cancel":
		res, cancelErr := c.CancelTask(ctx, card, f.task)
		if cancelErr != nil {
			return a2aClientFailure(out, errOut, verb, cancelErr, f.json)
		}
		record.TaskID = res.TaskID
		record.State = string(res.Task.Status.State)
	}

	if f.record != "" {
		if err := writeA2ARecord(f.record, record); err != nil {
			_, _ = fmt.Fprintf(errOut, "crier a2a-client %s: cannot write record %s: %v\n", verb, f.record, err)
			return a2aExitUsage
		}
	}
	if f.json {
		writeA2AJSON(out, record)
		return a2aExitOK
	}
	printA2ARecord(out, record, f.record)
	return a2aExitOK
}

// checkVerbFlags refuses a flag the chosen verb does not use. A silently
// ignored `-text` on a cancel would look like it had been sent.
func checkVerbFlags(verb string, fs *flag.FlagSet, f *a2aClientFlags) error {
	if strings.TrimSpace(f.url) == "" {
		return errors.New("-url is required (the peer origin, e.g. https://peer.example)")
	}
	if f.credFile != "" && f.credEnv != "" {
		return errors.New("declare the credential in exactly one place: -credential-file or -credential-env")
	}
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })

	switch verb {
	case "send":
		if f.text == "" {
			return errors.New("-text is required (this client sends a text part; richer parts come from the library API)")
		}
	case "get", "cancel":
		if strings.TrimSpace(f.task) == "" {
			return errors.New("-task is required (the task id)")
		}
		for _, name := range []string{"text", "message-id", "context-id"} {
			if set[name] {
				return fmt.Errorf("-%s belongs to the send verb, not %s", name, verb)
			}
		}
		if verb == "cancel" && set["wire"] {
			return errors.New("-wire belongs to send/get, not cancel")
		}
	default:
		for _, name := range []string{"text", "message-id", "context-id", "task"} {
			if set[name] {
				return fmt.Errorf("-%s belongs to another verb, not card", name)
			}
		}
	}
	return nil
}

// buildA2AClient assembles the client from the flags: the credential comes from
// a FILE or an ENVIRONMENT VARIABLE only, never from a flag value.
func buildA2AClient(f *a2aClientFlags, getenv config.Getenv) (*client.Client, error) {
	opts := client.Options{
		Origin:      f.url,
		AgentID:     f.agentID,
		Timeout:     f.timeout,
		Methods:     a2aWire(f.wire),
		Credentials: nil,
	}
	switch {
	case f.credFile != "":
		cred, err := client.CredentialFromFile(f.scheme, f.credFile)
		if err != nil {
			return nil, err
		}
		opts.Credentials = append(opts.Credentials, cred)
	case f.credEnv != "":
		cred, err := client.CredentialFromEnv(f.scheme, f.credEnv, getenv)
		if err != nil {
			return nil, err
		}
		opts.Credentials = append(opts.Credentials, cred)
	}
	return client.New(opts)
}

// a2aWire maps -wire onto the client's dialect. An unknown value is refused by
// the client's own configuration error rather than defaulted.
func a2aWire(name string) *client.Operations {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "legacy", "slash", "pre-1.0", "v0.3":
		w := client.WireLegacy
		return &w
	case "", "v1", "1", "1.0":
		w := client.WireV1
		return &w
	default:
		return &client.Operations{Name: name}
	}
}

// taskState is the state a send established: the task's own state, or — for a
// direct Message answer, which has no task to track (§3.1.1) — the fact that
// the peer answered inline.
func taskState(task *a2a.Task, message *a2a.Message) string {
	if message != nil {
		return "ANSWERED_INLINE"
	}
	if task == nil {
		return ""
	}
	return string(task.Status.State)
}

// writeA2ARecord writes the record as JSON, mode 0600. The record holds ids and
// a digest — no credential — and the mode keeps it out of other users' reach
// anyway.
func writeA2ARecord(path string, record a2aClientRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return os.WriteFile(path, raw, 0o600)
}

// writeA2AJSON prints one JSON object.
func writeA2AJSON(out io.Writer, v any) {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// printA2ARecord is the human report: what was addressed, and the ids the
// operator (or a later run) needs.
func printA2ARecord(out io.Writer, r a2aClientRecord, recordPath string) {
	_, _ = fmt.Fprintf(out, "%s %s via %s (%s binding %s)\n", r.Operation, r.CardName, r.Endpoint, a2aBindingName(), r.Wire)
	_, _ = fmt.Fprintf(out, "  card digest: %s\n", r.CardDigest)
	if r.CardETag != "" {
		_, _ = fmt.Fprintf(out, "  card etag:   %s\n", r.CardETag)
	}
	if r.Tenant != "" {
		_, _ = fmt.Fprintf(out, "  tenant:      %s\n", r.Tenant)
	}
	if r.MessageID != "" {
		_, _ = fmt.Fprintf(out, "  messageId:   %s\n", r.MessageID)
	}
	if r.TaskID != "" {
		_, _ = fmt.Fprintf(out, "  taskId:      %s\n", r.TaskID)
	}
	if r.State != "" {
		_, _ = fmt.Fprintf(out, "  state:       %s\n", r.State)
	}
	if r.Attempts > 0 {
		_, _ = fmt.Fprintf(out, "  attempts:    %d\n", r.Attempts)
	}
	if r.Replayed {
		_, _ = fmt.Fprintln(out, "  replay:      the peer served this messageId from its idempotency store (no new task)")
	}
	if recordPath != "" {
		_, _ = fmt.Fprintf(out, "  record:      %s\n", recordPath)
	} else {
		_, _ = fmt.Fprintln(out, "  record:      not written (pass -record PATH to keep it, or -json to print it)")
	}
}

// a2aBindingName is the one protocol binding crier speaks (§2).
func a2aBindingName() string { return "JSONRPC" }

// a2aClientFailure prints a failure in both of its forms and returns the exit
// code for its name. The credential is never part of a failure: the client's
// errors carry ids, statuses and the peer's own message only.
func a2aClientFailure(out, errOut io.Writer, op string, err error, asJSON bool) int {
	code := a2aExitCode(err)
	name := client.ErrorName(err)
	if name == "" {
		name = "UNCLASSIFIED_FAILURE"
	}
	detail := err.Error()
	status := 0
	var clientErr *client.Error
	if errors.As(err, &clientErr) {
		detail = clientErr.Detail
		status = clientErr.Status
		if detail == "" {
			detail = clientErr.Error()
		}
	}
	if asJSON {
		writeA2AJSON(out, a2aClientError{Operation: op, Name: name, Detail: detail, Status: status, ExitCode: code})
	}
	_, _ = fmt.Fprintf(errOut, "crier a2a-client %s: %s: %s\n", op, name, detail)
	return code
}

// printA2AClientUsage is the subcommand's own usage text.
func printA2AClientUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "crier a2a-client — talk to a foreign A2A agent (outbound; needs no local A2A switch)")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  crier a2a-client card   -url URL [-agent-id ID] [credential] [-json]")
	_, _ = fmt.Fprintln(out, "  crier a2a-client send   -url URL -text TEXT [-message-id ID] [-context-id ID] [credential] [-record PATH]")
	_, _ = fmt.Fprintln(out, "  crier a2a-client get    -url URL -task ID [credential] [-record PATH]")
	_, _ = fmt.Fprintln(out, "  crier a2a-client cancel -url URL -task ID [credential] [-record PATH]")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Discovery reads /.well-known/agent-card.json and validates it; a peer task id and the")
	_, _ = fmt.Fprintln(out, "card's sha256 digest are what the record keeps. -message-id IS the idempotency key:")
	_, _ = fmt.Fprintln(out, "a retry with the same value returns the SAME task instead of creating a second one.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Credential: -credential-file PATH (mode 0600) or -credential-env NAME. Never a flag value.")
	_, _ = fmt.Fprintln(out, "Flags: -url -agent-id -scheme -credential-file -credential-env -wire -record -json -timeout")
	_, _ = fmt.Fprintln(out, "       -text -message-id -context-id -task")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Exit codes: 0 ok · 1 unreachable · 2 usage · 3 no agent card (404) · 4 invalid JSON-RPC response")
	_, _ = fmt.Fprintln(out, "            5 unauthenticated (401) · 6 malformed card · 7 credential required by the card")
	_, _ = fmt.Fprintln(out, "            8 peer refused the operation · 9 peer HTTP error · 10 credential unreadable")
}
