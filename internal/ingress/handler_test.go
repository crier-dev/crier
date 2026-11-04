package ingress

// handler_test.go — the unit battery of CRIER-INGRESS §7 (1-9) plus the
// refusals the catalog names. The store is the REAL JSONL session store in a
// temp directory, not a double: the row's whole claim is that an ingress push
// writes the same records the session routes write, and a fake store would let
// a second append shape pass this battery unnoticed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/session"
)

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

type testEnv struct {
	handler *Handler
	repo    session.Repository
	root    string
	now     time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	repo, err := session.NewJSONLStore(root)
	if err != nil {
		t.Fatalf("open jsonl store: %v", err)
	}
	now := time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC)
	routes, err := NewJSONLRouteStore(filepath.Join(root, "routes"))
	if err != nil {
		t.Fatalf("open route store: %v", err)
	}
	h := &Handler{
		Repo:  repo,
		Route: routes,
		Idem:  NewMemoryIdempotencyStore(),
		Now:   func() time.Time { return now },
	}
	return &testEnv{handler: h, repo: repo, root: root, now: now}
}

// post issues one ingress request and returns the recorder.
func (e *testEnv) post(t *testing.T, body any, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	switch v := body.(type) {
	case string:
		reader = bytes.NewReader([]byte(v))
	case []byte:
		reader = bytes.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/ingress/messages", reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// createSession seeds a session with the named agent members through the same
// repository calls a create+join makes, so a mode=session test starts from a
// real room and real membership records.
func (e *testEnv) createSession(t *testing.T, id string, members ...string) *session.Session {
	t.Helper()
	ctx := context.Background()
	sess := &session.Session{
		ID:        id,
		Namespace: "",
		Kind:      session.KindChannel,
		Title:     "fixture",
		CreatedAt: e.now,
		CreatedBy: session.AuthorRef{Agent: "seed"},
		State:     session.SessionOpen,
	}
	if err := e.repo.CreateSession(ctx, sess, 1); err != nil {
		t.Fatalf("create session: %v", err)
	}
	seq := int64(1)
	for _, m := range members {
		seq++
		member := &session.Member{
			SessionID: id, MemberType: session.MemberAgent, MemberID: m,
			Role: session.RoleMember, AddedAt: e.now, AddedBy: "seed",
		}
		if err := e.repo.AddMember(ctx, member, seq, nil); err != nil {
			t.Fatalf("add member %s: %v", m, err)
		}
	}
	return sess
}

func decodeResult(t *testing.T, rec *httptest.ResponseRecorder) Result {
	t.Helper()
	var res Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result %q: %v", rec.Body.String(), err)
	}
	return res
}

func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

func (e *testEnv) transcript(t *testing.T, id string) *session.State {
	t.Helper()
	st, err := e.repo.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return st
}

func (e *testEnv) recordTypes(t *testing.T, id string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.root, id+".jsonl"))
	if err != nil {
		t.Fatalf("read session log %s: %v", id, err)
	}
	var out []string
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("parse record %q: %v", line, err)
		}
		out = append(out, rec.Type)
	}
	return out
}

func countType(types []string, want string) int {
	n := 0
	for _, typ := range types {
		if typ == want {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// §7 tests 1-2: the mode selector.
// ---------------------------------------------------------------------------

func TestIngressModeRequired(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, map[string]any{
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "no mode",
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeModeRequired {
		t.Fatalf("code = %q, want %q", code, CodeModeRequired)
	}
	// Nothing appended: no session log exists at all.
	entries, err := os.ReadDir(env.root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") && entry.Name() != "ingress-routes.jsonl" {
			t.Fatalf("a refused request wrote %s", entry.Name())
		}
	}
}

func TestIngressModeUnknownNotDefaulted(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, map[string]any{
		"mode":   "channel",
		"target": map[string]any{"session_id": "s_anything"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeModeUnknown {
		t.Fatalf("code = %q, want %q", code, CodeModeUnknown)
	}
	ids, err := env.repo.Sessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("sessions created = %v, want none", ids)
	}
}

func TestIngressModeDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.handler.AllowedModes = []Mode{ModeSession, ModeInbox}
	rec := env.post(t, map[string]any{
		"mode":   "new_session",
		"target": map[string]any{"template": map[string]any{"title": "x"}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeModeDisabled {
		t.Fatalf("code = %q, want %q", code, CodeModeDisabled)
	}
}

// ---------------------------------------------------------------------------
// §7 test 3: new_session creates and posts.
// ---------------------------------------------------------------------------

func TestIngressNewSessionCreatesAndPosts(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{
			"template": map[string]any{
				"title":     "nightly build triage",
				"kind":      "channel",
				"namespace": "",
				"members": []map[string]string{
					{"member_type": "agent", "member_id": "alpha"},
					{"member_type": "agent", "member_id": "ci-bot"},
				},
			},
		},
		"author": map[string]string{"agent": "ci-bot"},
		"kind":   "addressed",
		"text":   "build 4412 failed on main",
		"tags":   []string{"build", "urgent"},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	res := decodeResult(t, rec)
	if !res.CreatedSession {
		t.Fatal("created_session = false, want true")
	}
	if res.SessionID == "" || res.MessageID == "" || res.Seq == 0 {
		t.Fatalf("result is missing ids: %+v", res)
	}
	if res.Delivery.Mode != "store" || res.Delivery.State != "stored" {
		t.Fatalf("delivery = %+v, want store/stored (no harness adapter is wired)", res.Delivery)
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 (one per declared member)", len(res.Outcomes))
	}
	for _, o := range res.Outcomes {
		if o.Result != session.OutcomeAccepted || o.MemberType != string(session.MemberAgent) {
			t.Fatalf("outcome %+v, want an accepted agent result", o)
		}
	}

	types := env.recordTypes(t, res.SessionID)
	if got := countType(types, "session.create"); got != 1 {
		t.Fatalf("session.create records = %d, want 1 (%v)", got, types)
	}
	// The author auto-joins: alpha + ci-bot declared, ci-bot is the author, so
	// the declared pair is exactly the membership set and no duplicate add is
	// written for the sender.
	if got := countType(types, "session.member.add"); got != 2 {
		t.Fatalf("session.member.add records = %d, want 2 (%v)", got, types)
	}
	if got := countType(types, "session.message"); got != 1 {
		t.Fatalf("message records = %d, want 1 (%v)", got, types)
	}

	st := env.transcript(t, res.SessionID)
	if len(st.Messages) != 1 {
		t.Fatalf("transcript has %d messages, want 1", len(st.Messages))
	}
	msg := st.Messages[0]
	if msg.ID != res.MessageID || msg.Seq != res.Seq {
		t.Fatalf("transcript message %+v does not match the result %+v", msg, res)
	}
	if strings.Join(msg.Tags, ",") != "build,urgent" {
		t.Fatalf("stored tags = %v, want [build urgent] (CRIER-TAGS order preserved)", msg.Tags)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Text != "build 4412 failed on main" {
		t.Fatalf("payload.text = %q", payload.Text)
	}
	// The author is a member, so the session is readable by the sender.
	found := false
	for _, m := range st.Members {
		if m.MemberID == "ci-bot" && m.MemberType == session.MemberAgent {
			found = true
		}
	}
	if !found {
		t.Fatalf("author did not auto-join: members = %+v", st.Members)
	}
}

// ---------------------------------------------------------------------------
// §7 tests 4-5: idempotency.
// ---------------------------------------------------------------------------

func TestIngressNewSessionIdempotent(t *testing.T) {
	env := newTestEnv(t)
	body := map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title":   "triage",
			"members": []map[string]string{{"member_type": "agent", "member_id": "alpha"}},
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "same call twice",
	}
	header := map[string]string{IdempotencyHeader: "key-1"}

	first := env.post(t, body, header)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d (body %s)", first.Code, first.Body)
	}
	second := env.post(t, body, header)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want the original 201 (body %s)", second.Code, second.Body)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay body differs:\n first: %s\nsecond: %s", first.Body, second.Body)
	}

	ids, err := env.repo.Sessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("sessions = %v, want exactly one", ids)
	}
	st := env.transcript(t, ids[0])
	if len(st.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(st.Messages))
	}
}

func TestIngressKeyReusedDifferentBody(t *testing.T) {
	env := newTestEnv(t)
	header := map[string]string{IdempotencyHeader: "key-reused"}
	first := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title":   "one",
			"members": []map[string]string{{"member_type": "agent", "member_id": "alpha"}},
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "first",
	}, header)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d (body %s)", first.Code, first.Body)
	}
	second := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title":   "two",
			"members": []map[string]string{{"member_type": "agent", "member_id": "alpha"}},
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "second",
	}, header)
	if second.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", second.Code, second.Body)
	}
	if code := decodeErrorCode(t, second); code != CodeKeyReused {
		t.Fatalf("code = %q, want %q", code, CodeKeyReused)
	}
	ids, _ := env.repo.Sessions(context.Background())
	if len(ids) != 1 {
		t.Fatalf("sessions = %v, want one (the refused key created nothing)", ids)
	}
}

// ---------------------------------------------------------------------------
// §7 test 6: session mode membership.
// ---------------------------------------------------------------------------

func TestIngressSessionModeRequiresMembership(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	rec := env.post(t, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_room"},
		"author": map[string]string{"agent": "stranger"},
		"text":   "let me in",
	}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeAuthorNotMember {
		t.Fatalf("code = %q, want %q", code, CodeAuthorNotMember)
	}
	st := env.transcript(t, "s_room")
	if len(st.Messages) != 0 {
		t.Fatalf("messages = %d, want 0 (a refused author appends nothing)", len(st.Messages))
	}
}

func TestIngressSessionModePostsThroughTheSessionPath(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha", "beta")
	rec := env.post(t, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_room"},
		"author": map[string]string{"agent": "alpha"},
		"text":   "build 4412 failed on main",
		"tags":   []string{"build"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	res := decodeResult(t, rec)
	if res.SessionID != "s_room" || res.CreatedSession {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].MemberID != "beta" {
		t.Fatalf("outcomes = %+v, want the one other agent participant", res.Outcomes)
	}
	st := env.transcript(t, "s_room")
	if len(st.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(st.Messages))
	}
	msg := st.Messages[0]
	if msg.ID != res.MessageID {
		t.Fatalf("message id = %q, result = %q", msg.ID, res.MessageID)
	}
	if msg.ThreadID != msg.ID {
		t.Fatalf("thread id = %q, want the message's own id for a root", msg.ThreadID)
	}
	if len(msg.Audience.Targets) != 1 || msg.Audience.Targets[0].ID != "beta" {
		t.Fatalf("audience = %+v, want [beta]", msg.Audience)
	}
	if len(msg.Tags) != 1 || msg.Tags[0] != "build" {
		t.Fatalf("tags = %v", msg.Tags)
	}
}

// ---------------------------------------------------------------------------
// §7 test 7: inbox mode calls the inbox path unchanged.
// ---------------------------------------------------------------------------

type recordingInbox struct {
	calls   int
	agentID string
	capab   string
	msg     InboxMessage
	entryID string
	err     error
}

func (r *recordingInbox) Deliver(_ context.Context, agentID, capability string, msg InboxMessage) ([]session.DeliveryOutcome, error) {
	r.calls++
	r.agentID, r.capab, r.msg = agentID, capability, msg
	if r.err != nil {
		return nil, r.err
	}
	if r.entryID == "" {
		r.entryID = "entry-1"
	}
	return []session.DeliveryOutcome{{
		Target: agentID, Outcome: session.OutcomeAccepted, InboxEntryID: r.entryID, UpdatedAt: time.Now().UTC(),
	}}, nil
}

func TestIngressInboxModeCallsTheInboxPath(t *testing.T) {
	env := newTestEnv(t)
	inbox := &recordingInbox{entryID: "e_fixed"}
	env.handler.Inbox = inbox

	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "alpha"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "build 4412 failed on main",
		"tags":   []string{"build"},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	if inbox.calls != 1 {
		t.Fatalf("inbox calls = %d, want exactly 1", inbox.calls)
	}
	if inbox.agentID != "alpha" || inbox.capab != "" {
		t.Fatalf("inbox target = %q/%q", inbox.agentID, inbox.capab)
	}
	if inbox.msg.Author.Agent != "ci-bot" || len(inbox.msg.Tags) != 1 || inbox.msg.Tags[0] != "build" {
		t.Fatalf("inbox message = %+v, want the author and tags carried through", inbox.msg)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(inbox.msg.Payload, &payload); err != nil || payload.Text == "" {
		t.Fatalf("inbox payload = %s (%v)", inbox.msg.Payload, err)
	}
	res := decodeResult(t, rec)
	if res.MessageID != "e_fixed" {
		t.Fatalf("message_id = %q, want the inbox entry id the delivery path returned", res.MessageID)
	}
	if res.SessionID != "" {
		t.Fatalf("session_id = %q, want absent for mode=inbox", res.SessionID)
	}
	if res.CreatedSession {
		t.Fatal("created_session = true for an inbox push")
	}
	// The route record says where it landed (§3).
	raw, err := os.ReadFile(env.handler.Route.(*JSONLRouteStore).Path())
	if err != nil {
		t.Fatalf("read route log: %v", err)
	}
	var route RouteRecord
	if err := json.Unmarshal(bytes.TrimSpace(raw), &route); err != nil {
		t.Fatalf("decode route record %q: %v", raw, err)
	}
	if route.AgentID != "alpha" || route.MessageID != "e_fixed" || route.Author.Agent != "ci-bot" {
		t.Fatalf("route record = %+v", route)
	}
	if strings.Join(route.Tags, ",") != "build" {
		t.Fatalf("route tags = %v", route.Tags)
	}
	if strings.TrimSpace(route.ID) == "" {
		t.Fatal("route record has no id")
	}
}

func TestIngressInboxCapabilityForm(t *testing.T) {
	env := newTestEnv(t)
	inbox := &recordingInbox{entryID: "e_cap"}
	env.handler.Inbox = inbox
	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"capability": "build"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "for whoever holds build",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}
	if inbox.capab != "build" {
		t.Fatalf("capability = %q, want build", inbox.capab)
	}
}

func TestIngressInboxUnknownAgent(t *testing.T) {
	env := newTestEnv(t)
	env.handler.Inbox = &recordingInbox{err: fmt.Errorf("%w: agent %q is not registered", ErrAgentUnknown, "ghost")}
	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "ghost"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeAgentUnknown {
		t.Fatalf("code = %q, want %q", code, CodeAgentUnknown)
	}
}

// ---------------------------------------------------------------------------
// §7 test 8: refused bodies.
// ---------------------------------------------------------------------------

func TestIngressTaskKindRefused(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	rec := env.post(t, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_room"},
		"author": map[string]string{"agent": "alpha"},
		"kind":   "task",
		"text":   "do the thing",
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeTaskKindRefused {
		t.Fatalf("code = %q, want %q", code, CodeTaskKindRefused)
	}
	if st := env.transcript(t, "s_room"); len(st.Messages) != 0 {
		t.Fatalf("messages = %d, want 0", len(st.Messages))
	}
}

func TestIngressEmptyMessage(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	rec := env.post(t, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_room"},
		"author": map[string]string{"agent": "alpha"},
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeEmptyMessage {
		t.Fatalf("code = %q, want %q", code, CodeEmptyMessage)
	}
}

func TestIngressPayloadOnlyIsAStructuredMessage(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	rec := env.post(t, map[string]any{
		"mode":    "session",
		"target":  map[string]any{"session_id": "s_room"},
		"author":  map[string]string{"agent": "alpha"},
		"payload": map[string]any{"build": 4412, "status": "failed"},
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	st := env.transcript(t, "s_room")
	var payload map[string]any
	if err := json.Unmarshal(st.Messages[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["build"] != float64(4412) || payload["status"] != "failed" {
		t.Fatalf("payload = %v, want the caller's object verbatim", payload)
	}
}

func TestIngressBodyTooLarge(t *testing.T) {
	env := newTestEnv(t)
	env.handler.MaxBodyBytes = 128
	body := strings.Repeat("x", 512)
	rec := env.post(t, map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "alpha"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   body,
	}, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeBodyTooLarge {
		t.Fatalf("code = %q, want %q", code, CodeBodyTooLarge)
	}
}

func TestIngressUnknownTopLevelFieldRefused(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, `{"mode":"inbox","target":{"agent_id":"alpha"},"author":{"agent":"ci-bot"},"text":"hi","sessionid":"s_x"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeInvalidJSON {
		t.Fatalf("code = %q, want %q", code, CodeInvalidJSON)
	}
}

func TestIngressTagsInvalid(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	for _, taglist := range [][]string{{"Urgent"}, {"-lead"}, {"a:b:c"}, {"sys:internal"}} {
		rec := env.post(t, map[string]any{
			"mode":   "session",
			"target": map[string]any{"session_id": "s_room"},
			"author": map[string]string{"agent": "alpha"},
			"text":   "tagged",
			"tags":   taglist,
		}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("tags %v: status = %d, want 400 (body %s)", taglist, rec.Code, rec.Body)
		}
		if code := decodeErrorCode(t, rec); code != CodeTagsInvalid {
			t.Fatalf("tags %v: code = %q, want %q", taglist, code, CodeTagsInvalid)
		}
	}
}

func TestIngressSessionNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_missing"},
		"author": map[string]string{"agent": "alpha"},
		"text":   "hello",
	}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeSessionNotFound {
		t.Fatalf("code = %q, want %q", code, CodeSessionNotFound)
	}
}

func TestIngressTemplateInvalid(t *testing.T) {
	env := newTestEnv(t)
	cases := []map[string]any{
		{"title": "", "kind": "channel"},
		{"title": "x", "kind": "thread"},
		{"title": "x", "namespace": "undeclared-realm"},
		{"title": "x", "members": []map[string]string{{"member_type": "robot", "member_id": "a"}}},
		{"title": "x", "members": []map[string]string{{"member_type": "agent", "member_id": ""}}},
	}
	for _, tmpl := range cases {
		rec := env.post(t, map[string]any{
			"mode":   "new_session",
			"target": map[string]any{"template": tmpl},
			"author": map[string]string{"agent": "ci-bot"},
			"text":   "hello",
		}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("template %v: status = %d, want 400 (body %s)", tmpl, rec.Code, rec.Body)
		}
		if code := decodeErrorCode(t, rec); code != CodeTemplateInvalid {
			t.Fatalf("template %v: code = %q, want %q", tmpl, code, CodeTemplateInvalid)
		}
	}
	if ids, _ := env.repo.Sessions(context.Background()); len(ids) != 0 {
		t.Fatalf("sessions = %v, want none", ids)
	}
}

func TestIngressTemplateNamespaceIsHonoured(t *testing.T) {
	env := newTestEnv(t)
	reg, err := namespace.NewRegistry([]namespace.Policy{{Name: "crier", Auth: namespace.AuthShared}}, nil)
	if err != nil {
		t.Fatalf("namespace registry: %v", err)
	}
	env.handler.Namespaces = reg
	rec := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title": "triage", "namespace": "crier",
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	res := decodeResult(t, rec)
	st := env.transcript(t, res.SessionID)
	if st.Session.Namespace != "crier" {
		t.Fatalf("session namespace = %q, want crier", st.Session.Namespace)
	}
}

// ---------------------------------------------------------------------------
// §7 test 9: a partial member add leaves no message.
// ---------------------------------------------------------------------------

// failingRepo fails the Nth AddMember call, simulating a backend write that
// fails for the third of five members.
type failingRepo struct {
	session.Repository
	failAt int
	calls  int
}

func (f *failingRepo) AddMember(ctx context.Context, m *session.Member, seq int64, share *session.ContextShare) error {
	f.calls++
	if f.calls == f.failAt {
		return fmt.Errorf("injected store failure for %s", m.MemberID)
	}
	return f.Repository.AddMember(ctx, m, seq, share)
}

func TestIngressPartialMemberAddLeavesNoMessage(t *testing.T) {
	env := newTestEnv(t)
	repo := &failingRepo{Repository: env.handler.Repo, failAt: 4}
	env.handler.Repo = repo

	rec := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title": "five",
			"members": []map[string]string{
				{"member_type": "agent", "member_id": "m1"},
				{"member_type": "agent", "member_id": "m2"},
				{"member_type": "agent", "member_id": "m3"},
				{"member_type": "agent", "member_id": "m4"},
				{"member_type": "agent", "member_id": "m5"},
			},
		}},
		"author":          map[string]string{"agent": "ci-bot"},
		"text":            "must not land",
		"idempotency_key": "partial-1",
	}, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeStoreError {
		t.Fatalf("code = %q, want %q", code, CodeStoreError)
	}
	if !strings.Contains(rec.Body.String(), "m3") {
		t.Fatalf("refusal does not name the member: %s", rec.Body)
	}

	ids, err := env.repo.Sessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("sessions = %v, want the one the create wrote", ids)
	}
	st := env.transcript(t, ids[0])
	if len(st.Messages) != 0 {
		t.Fatalf("messages = %d, want 0: a partial member add must not append the message", len(st.Messages))
	}
	// The author auto-join and the two members that succeeded are real records.
	agents := map[string]bool{}
	for _, m := range st.Members {
		if m.MemberType == session.MemberAgent {
			agents[m.MemberID] = true
		}
	}
	for _, want := range []string{"ci-bot", "m1", "m2"} {
		if !agents[want] {
			t.Fatalf("member %q is missing after the partial add (members = %+v)", want, st.Members)
		}
	}
	if agents["m3"] {
		t.Fatalf("member m3 was recorded despite the injected failure")
	}

	// The key was NOT recorded: a retry must resume, not replay a 500.
	if _, ok, _ := env.handler.Idem.Lookup(context.Background(), "", "partial-1"); ok {
		t.Fatal("the idempotency key was recorded for a failed call; a retry would replay the failure")
	}
}

// ---------------------------------------------------------------------------
// reply_to_thread.
// ---------------------------------------------------------------------------

func TestIngressReplyToThreadKeepsTheThread(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	first := env.post(t, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": "s_room"},
		"author": map[string]string{"agent": "alpha"},
		"text":   "root",
	}, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("root status = %d (body %s)", first.Code, first.Body)
	}
	rootRes := decodeResult(t, first)

	second := env.post(t, map[string]any{
		"mode":            "session",
		"target":          map[string]any{"session_id": "s_room"},
		"author":          map[string]string{"agent": "alpha"},
		"text":            "reply",
		"reply_to_thread": rootRes.MessageID,
	}, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("reply status = %d (body %s)", second.Code, second.Body)
	}
	replyRes := decodeResult(t, second)
	st := env.transcript(t, "s_room")
	reply := st.Message(replyRes.MessageID)
	if reply == nil {
		t.Fatal("reply is not in the transcript")
	}
	if reply.ThreadID != rootRes.MessageID {
		t.Fatalf("reply thread = %q, want the root thread %q", reply.ThreadID, rootRes.MessageID)
	}
	if reply.ParentID != rootRes.MessageID {
		t.Fatalf("reply parent = %q, want the thread root %q (a reply never deepens a thread)", reply.ParentID, rootRes.MessageID)
	}
}

func TestIngressReplyToUnknownThread(t *testing.T) {
	env := newTestEnv(t)
	env.createSession(t, "s_room", "alpha")
	rec := env.post(t, map[string]any{
		"mode":            "session",
		"target":          map[string]any{"session_id": "s_room"},
		"author":          map[string]string{"agent": "alpha"},
		"text":            "reply",
		"reply_to_thread": "m_nope",
	}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeReplyTooDeep {
		t.Fatalf("code = %q, want %q", code, CodeReplyTooDeep)
	}
}

func TestIngressReplyToThreadOnNewSession(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, map[string]any{
		"mode":            "new_session",
		"target":          map[string]any{"template": map[string]any{"title": "fresh"}},
		"author":          map[string]string{"agent": "ci-bot"},
		"text":            "reply",
		"reply_to_thread": "m_root",
	}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	if code := decodeErrorCode(t, rec); code != CodeReplyTooDeep {
		t.Fatalf("code = %q, want %q", code, CodeReplyTooDeep)
	}
}

// ---------------------------------------------------------------------------
// Harness dispatch hook (§6: best-effort, reported honestly).
// ---------------------------------------------------------------------------

type fakeDispatch struct {
	calls  int
	report DeliveryReport
	err    error
}

func (f *fakeDispatch) Dispatch(_ context.Context, _ DispatchRequest) (DeliveryReport, error) {
	f.calls++
	return f.report, f.err
}

func TestIngressDispatchesWhenAnAdapterIsConfigured(t *testing.T) {
	env := newTestEnv(t)
	dispatch := &fakeDispatch{report: DeliveryReport{Mode: "harness", Harness: "opencode", State: "dispatched"}}
	env.handler.Dispatch = dispatch
	rec := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title":   "triage",
			"members": []map[string]string{{"member_type": "agent", "member_id": "alpha"}},
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "dispatch me",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body)
	}
	res := decodeResult(t, rec)
	if dispatch.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", dispatch.calls)
	}
	if res.Delivery.State != "dispatched" || res.Delivery.Harness != "opencode" {
		t.Fatalf("delivery = %+v", res.Delivery)
	}
}

func TestIngressDispatchFailureIsReportedNotRolledBack(t *testing.T) {
	env := newTestEnv(t)
	env.handler.Dispatch = &fakeDispatch{err: fmt.Errorf("adapter unreachable")}
	rec := env.post(t, map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title":   "triage",
			"members": []map[string]string{{"member_type": "agent", "member_id": "alpha"}},
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "dispatch me",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: storage is the contract (§6)", rec.Code)
	}
	res := decodeResult(t, rec)
	if res.Delivery.State != "failed" || res.Delivery.Error == "" {
		t.Fatalf("delivery = %+v, want a failed report naming the error", res.Delivery)
	}
	st := env.transcript(t, res.SessionID)
	if len(st.Messages) != 1 {
		t.Fatalf("messages = %d, want the message to stay stored", len(st.Messages))
	}
}

// ---------------------------------------------------------------------------
// Catalog integrity.
// ---------------------------------------------------------------------------

func TestCatalogCoversEveryDeclaredCode(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Catalog {
		if e.Code == "" || e.Status == 0 {
			t.Fatalf("catalog entry %+v has no code or status", e)
		}
		if seen[e.Code] {
			t.Fatalf("code %q is declared twice", e.Code)
		}
		seen[e.Code] = true
	}
	// The §5 table's own vocabulary, verbatim, must all be present.
	for _, code := range []string{
		"INGRESS_INVALID_JSON", "INGRESS_MODE_REQUIRED", "INGRESS_MODE_UNKNOWN",
		"INGRESS_MODE_DISABLED", "INGRESS_SESSION_REQUIRED", "INGRESS_TEMPLATE_REQUIRED",
		"INGRESS_TEMPLATE_INVALID", "INGRESS_AGENT_REQUIRED", "INGRESS_AGENT_UNKNOWN",
		"INGRESS_AUTHOR_REQUIRED", "INGRESS_AUTHOR_NOT_MEMBER", "INGRESS_TASK_KIND_REFUSED",
		"INGRESS_REPLY_TOO_DEEP", "INGRESS_SESSION_NOT_FOUND", "INGRESS_KEY_REUSED",
		"INGRESS_BODY_TOO_LARGE", "INGRESS_TAGS_INVALID", "INGRESS_STORE_ERROR",
	} {
		if !seen[code] {
			t.Fatalf("§5 code %q is not in the catalog", code)
		}
	}
}

func TestRequestHashIsStableAndSensitive(t *testing.T) {
	base := &Request{Mode: ModeInbox, AgentID: "alpha", Author: session.AuthorRef{Agent: "ci"}, Text: "hi", Tags: []string{"a"}}
	h1, err := requestHash(base, "")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	h2, _ := requestHash(base, "")
	if h1 != h2 {
		t.Fatal("the same request hashed twice differs")
	}
	changed := *base
	changed.Text = "hi!"
	h3, _ := requestHash(&changed, "")
	if h1 == h3 {
		t.Fatal("a changed text hashed alike")
	}
	realmChange, _ := requestHash(base, "crier")
	if h1 == realmChange {
		t.Fatal("a changed realm hashed alike")
	}
}
