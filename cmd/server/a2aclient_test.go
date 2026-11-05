package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crier-dev/crier/internal/a2a"
)

// INT-A2A-007's CLI gate: `crier a2a-client` booted for real against an
// httptest peer. Every assertion here is about the SHIPPED surface — exit
// codes, the stderr/stderr text, and the record file — not about the client
// package's internals (internal/a2a/client owns those).

// a2aCLIPeer is a minimal crier-shaped A2A peer: it serves the publisher's own
// card projection and answers the three operations the CLI sends.
type a2aCLIPeer struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	card         []byte
	cardStatus   int
	rpcStatus    int
	rpcBody      string
	requireAuth  string
	cardAuth     bool
	rpcHits      int
	pathHits     []string
	tasks        map[string]string
	next         int
	lastAuthSeen string
}

func newA2ACLIPeer(t *testing.T) *a2aCLIPeer {
	t.Helper()
	p := &a2aCLIPeer{t: t, tasks: map[string]string{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	p.setCard(a2a.ServerInfo{Origin: p.srv.URL, Version: "test"})
	return p
}

func (p *a2aCLIPeer) setCard(srv a2a.ServerInfo) {
	card := a2a.BuildCard(a2a.CardRow{AgentID: "agent-b", Capabilities: []string{"solver"}}, srv)
	raw, err := json.Marshal(card)
	if err != nil {
		p.t.Fatalf("marshal card: %v", err)
	}
	p.mu.Lock()
	p.card = raw
	p.mu.Unlock()
}

func (p *a2aCLIPeer) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.pathHits = append(p.pathHits, r.URL.Path)
	p.lastAuthSeen = r.Header.Get("Authorization")
	card, cardStatus, cardAuth := p.card, p.cardStatus, p.cardAuth
	requireAuth := p.requireAuth
	p.mu.Unlock()

	if r.URL.Path == a2a.AgentCardPath {
		if cardAuth && r.Header.Get("Authorization") != "Bearer "+requireAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if cardStatus != 0 {
			w.WriteHeader(cardStatus)
			return
		}
		w.Header().Set("Content-Type", a2a.CardMediaType)
		w.Header().Set("ETag", a2a.CardETag(card))
		_, _ = w.Write(card)
		return
	}
	if r.URL.Path != a2a.JSONRPCBindingPath {
		http.NotFound(w, r)
		return
	}

	p.mu.Lock()
	p.rpcHits++
	rpcStatus, rpcBody := p.rpcStatus, p.rpcBody
	p.mu.Unlock()

	if requireAuth != "" && r.Header.Get("Authorization") != "Bearer "+requireAuth {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if rpcStatus != 0 {
		w.WriteHeader(rpcStatus)
		return
	}
	if rpcBody != "" {
		w.Header().Set("Content-Type", a2a.RPCMediaType)
		_, _ = io.WriteString(w, rpcBody)
		return
	}

	body, _ := io.ReadAll(r.Body)
	req, rpcErr := a2a.DecodeRequest(body)
	if rpcErr != nil {
		p.write(w, a2a.ErrorResponse(nil, rpcErr))
		return
	}
	switch req.Method {
	case a2a.MethodSendMessage, "message/send":
		params, rpcErr := a2a.DecodeSendMessageParams(req.Params)
		if rpcErr != nil {
			p.write(w, a2a.ErrorResponse(req.ID, rpcErr))
			return
		}
		p.mu.Lock()
		taskID, ok := p.tasks[params.Message.MessageID]
		if !ok {
			p.next++
			taskID = fmt.Sprintf("task-%d", p.next)
			p.tasks[params.Message.MessageID] = taskID
		}
		p.mu.Unlock()
		task := a2a.Task{
			ID:        taskID,
			ContextID: params.Message.ContextID,
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
			Metadata:  map[string]any{"crier": map[string]any{"idempotent_replay": ok}},
		}
		p.write(w, a2a.SuccessResponse(req.ID, a2a.SendMessageResponse{Task: &task}))
	case a2a.MethodGetTask, "tasks/get":
		params, rpcErr := a2a.DecodeGetTaskParams(req.Params)
		if rpcErr != nil {
			p.write(w, a2a.ErrorResponse(req.ID, rpcErr))
			return
		}
		p.write(w, a2a.SuccessResponse(req.ID, a2a.Task{ID: params.ID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}))
	case a2a.MethodCancelTask, "tasks/cancel":
		params, rpcErr := a2a.DecodeCancelTaskParams(req.Params)
		if rpcErr != nil {
			p.write(w, a2a.ErrorResponse(req.ID, rpcErr))
			return
		}
		p.write(w, a2a.SuccessResponse(req.ID, a2a.Task{ID: params.ID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}))
	default:
		p.write(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeMethodNotFound, "Method not found: "+req.Method)))
	}
}

func (p *a2aCLIPeer) write(w http.ResponseWriter, resp a2a.RPCResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		p.t.Fatalf("marshal rpc response: %v", err)
	}
	w.Header().Set("Content-Type", a2a.RPCMediaType)
	_, _ = w.Write(raw)
}

// sha256Of is the digest the record is asserted against.
func sha256Of(body []byte) [32]byte { return sha256.Sum256(body) }

func (p *a2aCLIPeer) cardBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.card
}

func (p *a2aCLIPeer) rpcCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rpcHits
}

// runCLI invokes the subcommand in-process, the way run() dispatches it.
func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = runA2AClient(args, &out, &errOut, func(string) string { return "" })
	return code, out.String(), errOut.String()
}

// writeA2ACLIFile writes a fixture (a credential or a record path's content).
func writeA2ACLIFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

// TestA2AClientSendRecordsPeerTaskAndCardDigest is the artifact criterion: the
// shipped subcommand reaches a real peer, and what it leaves behind names the
// peer's task id and the digest of the card it addressed.
func TestA2AClientSendRecordsPeerTaskAndCardDigest(t *testing.T) {
	p := newA2ACLIPeer(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record.json")

	code, stdout, stderr := runCLI("send", "-url", p.srv.URL, "-text", "deploy the canary", "-record", record)
	if code != 0 {
		t.Fatalf("send exit = %d, want 0 (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "task-1") {
		t.Errorf("stdout does not name the peer task id:\n%s", stdout)
	}

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var got struct {
		Operation  string `json:"operation"`
		Origin     string `json:"origin"`
		Endpoint   string `json:"endpoint"`
		CardDigest string `json:"card_digest"`
		CardName   string `json:"card_name"`
		MessageID  string `json:"message_id"`
		TaskID     string `json:"task_id"`
		State      string `json:"state"`
		Replayed   bool   `json:"replayed"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("record is not JSON: %v\n%s", err, raw)
	}
	if got.TaskID != "task-1" {
		t.Errorf("record task_id = %q, want task-1", got.TaskID)
	}
	if want := "sha256:" + fmt.Sprintf("%x", sha256Of(p.cardBytes())); got.CardDigest != want {
		t.Errorf("record card_digest = %q, want %q (sha256 of the served card)", got.CardDigest, want)
	}
	if got.CardName != "agent-b" || got.Origin != p.srv.URL {
		t.Errorf("record names %q at %q, want agent-b at %s", got.CardName, got.Origin, p.srv.URL)
	}
	if got.Endpoint != p.srv.URL+a2a.JSONRPCBindingPath {
		t.Errorf("record endpoint = %q, want the advertised binding URL", got.Endpoint)
	}
	if got.MessageID == "" {
		t.Error("record has no message_id — a retry would not be reproducible")
	}
	if got.State != string(a2a.TaskStateSubmitted) {
		t.Errorf("record state = %q, want TASK_STATE_SUBMITTED", got.State)
	}
	if got.Replayed {
		t.Error("a first send was recorded as replayed")
	}
	if info, err := os.Stat(record); err == nil && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("record mode = %04o, want 0600", info.Mode().Perm())
	}

	// The SAME -message-id is the same task, and the record says so — the
	// retry-safety half of the criterion, through the shipped CLI.
	code, stdout, stderr = runCLI("send", "-url", p.srv.URL, "-text", "deploy the canary",
		"-message-id", got.MessageID, "-record", record)
	if code != 0 {
		t.Fatalf("retried send exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	raw, err = os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var again struct {
		TaskID   string `json:"task_id"`
		Replayed bool   `json:"replayed"`
	}
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if again.TaskID != got.TaskID {
		t.Errorf("the retry produced task %q, want the original %q", again.TaskID, got.TaskID)
	}
	if !again.Replayed {
		t.Error("the retry was not recorded as replayed")
	}
	if !strings.Contains(stdout, "no new task") {
		t.Errorf("the human report does not state the replay:\n%s", stdout)
	}

	// A different messageId is a different task, and the record says which id
	// it used.
	code, _, stderr = runCLI("send", "-url", p.srv.URL, "-text", "deploy the canary", "-message-id", "other-id", "-record", record)
	if code != 0 {
		t.Fatalf("third send exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	raw, _ = os.ReadFile(record)
	var third struct {
		TaskID    string `json:"task_id"`
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &third); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if third.TaskID == got.TaskID {
		t.Errorf("a different messageId reused task %q", third.TaskID)
	}
	if third.MessageID != "other-id" {
		t.Errorf("record message_id = %q, want other-id", third.MessageID)
	}
}

// TestA2AClientCardAndTaskVerbs covers the other three verbs.
func TestA2AClientCardAndTaskVerbs(t *testing.T) {
	p := newA2ACLIPeer(t)
	dir := t.TempDir()

	code, stdout, stderr := runCLI("card", "-url", p.srv.URL)
	if code != 0 {
		t.Fatalf("card exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "sha256:") || !strings.Contains(stdout, "agent-b") {
		t.Errorf("card output lacks the name or the digest:\n%s", stdout)
	}

	code, stdout, stderr = runCLI("get", "-url", p.srv.URL, "-task", "task-9", "-json")
	if code != 0 {
		t.Fatalf("get exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"task_id": "task-9"`) || !strings.Contains(stdout, string(a2a.TaskStateWorking)) {
		t.Errorf("get output does not report task-9 in WORKING:\n%s", stdout)
	}

	record := filepath.Join(dir, "cancel.json")
	code, stdout, stderr = runCLI("cancel", "-url", p.srv.URL, "-task", "task-9", "-record", record)
	if code != 0 {
		t.Fatalf("cancel exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, string(a2a.TaskStateCanceled)) {
		t.Errorf("cancel output does not report the canceled state:\n%s", stdout)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec struct {
		Operation string `json:"operation"`
		TaskID    string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if rec.Operation != "cancel" || rec.TaskID != "task-9" {
		t.Errorf("record = %+v, want operation cancel for task-9", rec)
	}
}

// TestA2AClientFourFailuresHaveDistinctExitCodes is the acceptance criterion at
// the shipped surface: each of the four failure paths is non-zero, and no two
// share a code or a name.
func TestA2AClientFourFailuresHaveDistinctExitCodes(t *testing.T) {
	// An origin that was listening a moment ago and is now closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadOrigin := "http://" + ln.Addr().String()
	_ = ln.Close()

	card404 := newA2ACLIPeer(t)
	card404.cardStatus = http.StatusNotFound

	badRPC := newA2ACLIPeer(t)
	badRPC.rpcBody = `<html>definitely not a JSON-RPC response</html>`

	unauth := newA2ACLIPeer(t)
	unauth.requireAuth = "s3cret"

	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantName string
	}{
		{"unreachable", []string{"card", "-url", deadOrigin}, a2aExitUnreachable, "PEER_UNREACHABLE"},
		{"card404", []string{"card", "-url", card404.srv.URL}, a2aExitCardNotFound, "AGENT_CARD_NOT_FOUND"},
		{"invalidJSONRPC", []string{"send", "-url", badRPC.srv.URL, "-text", "x"}, a2aExitInvalidResponse, "INVALID_JSONRPC_RESPONSE"},
		{"unauthenticated", []string{"send", "-url", unauth.srv.URL, "-text", "x"}, a2aExitUnauthenticated, "UNAUTHENTICATED"},
	}
	codes := map[int]string{}
	names := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(append(tc.args, "-json")...)
			if code == 0 {
				t.Fatalf("exit = 0, want non-zero (stdout: %s)", stdout)
			}
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d (stderr: %s)", code, tc.wantCode, stderr)
			}
			if !strings.Contains(stderr, tc.wantName) {
				t.Errorf("stderr does not name %s:\n%s", tc.wantName, stderr)
			}
			var reported struct {
				Name     string `json:"name"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(stdout), &reported); err != nil {
				t.Fatalf("-json output is not a JSON object: %v\n%s", err, stdout)
			}
			if reported.Name != tc.wantName || reported.ExitCode != tc.wantCode {
				t.Errorf("-json reported %+v, want %s / %d", reported, tc.wantName, tc.wantCode)
			}
			if prev, seen := codes[code]; seen {
				t.Errorf("%s shares exit code %d with %s", tc.name, code, prev)
			}
			codes[code] = tc.name
			if prev, seen := names[tc.wantName]; seen {
				t.Errorf("%s shares its name with %s", tc.name, prev)
			}
			names[tc.wantName] = tc.name
		})
	}

	// The 401 path must not become a retry storm: exactly one JSON-RPC POST.
	if hits := unauth.rpcCount(); hits != 1 {
		t.Errorf("the unauthenticated peer saw %d JSON-RPC requests, want exactly 1", hits)
	}
}

// TestA2AClientCredentialRequiredRefusesBeforeSending covers the card-declared
// posture: a peer whose card requires bearerAuth is refused without a
// credential, BEFORE anything is sent.
func TestA2AClientCredentialRequiredRefusesBeforeSending(t *testing.T) {
	p := newA2ACLIPeer(t)
	p.setCard(a2a.ServerInfo{Origin: p.srv.URL, Version: "test", AuthTokenSet: true})

	code, stdout, stderr := runCLI("send", "-url", p.srv.URL, "-text", "x", "-json")
	if code != a2aExitCredentialRequired {
		t.Fatalf("exit = %d, want %d (stdout: %s, stderr: %s)", code, a2aExitCredentialRequired, stdout, stderr)
	}
	if !strings.Contains(stderr, "CREDENTIAL_REQUIRED") || !strings.Contains(stderr, "bearerAuth") {
		t.Errorf("stderr does not name the failure and the required scheme:\n%s", stderr)
	}
	if hits := p.rpcCount(); hits != 0 {
		t.Errorf("the peer saw %d JSON-RPC requests, want 0 — the refusal must precede the send", hits)
	}
}

// TestA2AClientCredentialFromFileAndEnv covers the two permitted sources, and
// that neither the report nor an error ever carries the secret.
func TestA2AClientCredentialFromFileAndEnv(t *testing.T) {
	p := newA2ACLIPeer(t)
	p.requireAuth = "token-abc"
	p.cardAuth = true
	p.setCard(a2a.ServerInfo{Origin: p.srv.URL, Version: "test", AuthTokenSet: true})

	dir := t.TempDir()
	credPath := writeA2ACLIFile(t, dir, "cred", "token-abc\n", 0o600)
	code, stdout, stderr := runCLI("send", "-url", p.srv.URL, "-text", "x", "-credential-file", credPath)
	if code != 0 {
		t.Fatalf("credential-file send exit = %d (stderr: %s)", code, stderr)
	}

	// The env source goes through the injected lookup, exactly as run() passes
	// config's getenv.
	var out, errOut bytes.Buffer
	code = runA2AClient([]string{"send", "-url", p.srv.URL, "-text", "y", "-credential-env", "CRIER_A2A_TOKEN"},
		&out, &errOut, func(name string) string {
			if name == "CRIER_A2A_TOKEN" {
				return "token-abc"
			}
			return ""
		})
	if code != 0 {
		t.Fatalf("credential-env send exit = %d (stderr: %s)", code, errOut.String())
	}
	if strings.Contains(stdout+stderr+out.String()+errOut.String(), "token-abc") {
		t.Error("a credential value reached the output")
	}

	// A world-readable credential file is refused by name.
	loose := writeA2ACLIFile(t, dir, "loose", "token-abc", 0o644)
	code, _, stderr = runCLI("card", "-url", p.srv.URL, "-credential-file", loose)
	if code != a2aExitCredentialInvalid {
		t.Errorf("world-readable credential exit = %d, want %d (stderr: %s)", code, a2aExitCredentialInvalid, stderr)
	}
}

// TestA2AClientUsageRefusals covers the argument shapes the subcommand refuses,
// each with the usage exit code and no network activity.
func TestA2AClientUsageRefusals(t *testing.T) {
	p := newA2ACLIPeer(t)
	cases := map[string][]string{
		"noVerb":          {},
		"unknownVerb":     {"frobnicate", "-url", p.srv.URL},
		"noURL":           {"card"},
		"sendWithoutText": {"send", "-url", p.srv.URL},
		"getWithoutTask":  {"get", "-url", p.srv.URL},
		"textOnGet":       {"get", "-url", p.srv.URL, "-task", "t", "-text", "x"},
		"taskOnCard":      {"card", "-url", p.srv.URL, "-task", "t"},
		"twoCredential":   {"card", "-url", p.srv.URL, "-credential-file", "a", "-credential-env", "B"},
		"unknownFlag":     {"card", "-url", p.srv.URL, "-nope"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := runCLI(args...)
			if code != a2aExitUsage {
				t.Fatalf("exit = %d, want %d for a refused argument shape (stderr: %s)", code, a2aExitUsage, stderr)
			}
		})
	}
	if hits := p.rpcCount(); hits != 0 {
		t.Errorf("a refused argument shape reached the peer %d times", hits)
	}
}
