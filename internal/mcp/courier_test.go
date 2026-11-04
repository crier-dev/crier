package mcp

// courier_test.go — the CRIER-MCP-NUDGE acceptance criteria (§7). The fixture
// drives the REAL registry store, the REAL session repository and the REAL read
// cursors: a nudge that only counts correctly against a fake counts nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/reads"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
)

const (
	nudgeAgent  = "alpha"
	nudgeOther  = "bravo"
	nudgeSecret = "TOP-SECRET-MESSAGE-BODY"
	nudgeInbox  = "TOP-SECRET-INBOX-BODY"
)

// nudgeFixture is one courier world: a registry with alpha's inbox holding two
// messages, and a session where alpha is a member and bravo has posted messages
// carrying tags and text.
type nudgeFixture struct {
	server    *MCPServer
	store     *registry.MemoryStore
	sessionID string
	// sessionUnread is how many of bravo's messages alpha has not read.
	sessionUnread int
}

// newNudgeFixture builds the world. msgCount is the number of unread session
// messages bravo posts; the FIRST carries tag "build", the second is untagged,
// and the rest carry "urgent".
func newNudgeFixture(t *testing.T, msgCount int) *nudgeFixture {
	t.Helper()
	ctx := context.Background()

	store := registry.NewMemoryStore()
	for _, id := range []string{nudgeAgent, nudgeOther} {
		if err := store.Register(&registry.Agent{ID: id, Capabilities: []string{}}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := store.Deliver(nudgeAgent, &registry.InboxEntry{
			Payload: []byte(`{"text":"` + nudgeInbox + `"}`),
		}); err != nil {
			t.Fatalf("deliver inbox message: %v", err)
		}
	}

	repo, err := session.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	cursors, err := reads.NewSQLStore(ctx, repo.DB(), reads.DialectSQLite)
	if err != nil {
		t.Fatalf("cursor store: %v", err)
	}
	t.Cleanup(func() { _ = cursors.Close() })
	svc := &reads.Service{Repo: repo, Cursors: cursors}

	const sid = "s_nudge"
	base := time.Now().UTC().Add(-time.Hour)
	if err := repo.CreateSession(ctx, &session.Session{
		ID: sid, Kind: session.KindChannel, CreatedAt: base, CreatedBy: session.AuthorRef{Agent: nudgeAgent},
	}, 1); err != nil {
		t.Fatalf("create session: %v", err)
	}
	seq := int64(2)
	for _, member := range []string{nudgeAgent, nudgeOther} {
		if err := repo.AddMember(ctx, &session.Member{
			SessionID: sid, MemberType: session.MemberAgent, MemberID: member,
			Role: session.RoleMember, AddedAt: base,
		}, seq, nil); err != nil {
			t.Fatalf("add member %s: %v", member, err)
		}
		seq++
	}
	for i := 0; i < msgCount; i++ {
		id := fmt.Sprintf("msg_%02d", i+1)
		tags := []string{"urgent"}
		switch i {
		case 0:
			tags = []string{"build"}
		case 1:
			tags = nil
		}
		if err := repo.PostMessage(ctx, &session.Message{
			ID: id, ThreadID: id, SessionID: sid, Kind: session.MessagePlain,
			Author:   session.AuthorRef{Agent: nudgeOther},
			Payload:  json.RawMessage(`{"text":"` + nudgeSecret + `"}`),
			Audience: session.Audience{Rule: session.AudienceSession},
			Tags:     tags,
			Seq:      seq, CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("post message %s: %v", id, err)
		}
		seq++
	}
	// One message from alpha itself: it is never its own unread mail.
	if err := repo.PostMessage(ctx, &session.Message{
		ID: "msg_own", ThreadID: "msg_own", SessionID: sid, Kind: session.MessagePlain,
		Author:   session.AuthorRef{Agent: nudgeAgent},
		Payload:  json.RawMessage(`{"text":"my own message"}`),
		Audience: session.Audience{Rule: session.AudienceSession},
		Tags:     []string{"urgent"},
		Seq:      seq, CreatedAt: base.Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("post own message: %v", err)
	}

	server := NewWithOptions(store, Options{
		AgentID: nudgeAgent,
		Courier: &Courier{Inbox: store, Sessions: repo, Reads: svc},
	})
	return &nudgeFixture{server: server, store: store, sessionID: sid, sessionUnread: msgCount}
}

// summary calls the tool handler and decodes its body.
func (f *nudgeFixture) summary(t *testing.T, args string) (UnreadSummary, error) {
	t.Helper()
	raw, err := f.server.handleCourierUnreadSummary(json.RawMessage(args))
	if err != nil {
		return UnreadSummary{}, err
	}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var out UnreadSummary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return out, nil
}

// TestUnreadSummaryShape pins §7.3: total, by_category, sessions and oldest_at
// are all populated, and the body contains NO message text — not the inbox
// body, not the session body, and no `text` key at all.
func TestUnreadSummaryShape(t *testing.T) {
	f := newNudgeFixture(t, 3)
	summary, err := f.summary(t, `{}`)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	// 2 agent-inbox messages + 3 unread session messages (alpha's own excluded).
	if summary.Total != 5 {
		t.Fatalf("total = %d, want 5", summary.Total)
	}
	// build (1) + urgent (1: the third message; the own message is excluded)
	// + uncategorized (2 inbox + 1 untagged session = 3).
	wantCategories := map[string]int{"build": 1, "urgent": 1, Uncategorized: 3}
	if len(summary.ByCategory) != len(wantCategories) {
		t.Fatalf("by_category = %v, want %v", summary.ByCategory, wantCategories)
	}
	for category, want := range wantCategories {
		if got := summary.ByCategory[category]; got != want {
			t.Fatalf("by_category[%q] = %d, want %d (all: %v)", category, got, want, summary.ByCategory)
		}
	}
	if len(summary.Sessions) != 1 || summary.Sessions[0].SessionID != f.sessionID || summary.Sessions[0].Unread != 3 {
		t.Fatalf("sessions = %+v, want one row for %q with 3 unread", summary.Sessions, f.sessionID)
	}
	if summary.OldestAt == nil {
		t.Fatal("oldest_at must be populated when something is unread")
	}

	// §7.3's leak assertion: marshal the response and grep it for the bodies.
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{nudgeSecret, nudgeInbox} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the summary leaked message text %q: %s", secret, raw)
		}
	}
	if strings.Contains(string(raw), `"text"`) {
		t.Fatalf("the summary must never carry a text key: %s", raw)
	}
}

// TestSummaryUnknownAgentIsEmptyNotError pins §7.4 and §5's NUDGE_AGENT_UNKNOWN
// rule: an unregistered agent gets total:0, not an MCP error.
func TestSummaryUnknownAgentIsEmptyNotError(t *testing.T) {
	f := newNudgeFixture(t, 2)
	f.server.agentID = "" // no bridge identity: an explicit agent is the only scope
	f.server.operatorScope = true

	summary, err := f.summary(t, `{"agent_id":"ghost"}`)
	if err != nil {
		t.Fatalf("an unknown agent must not be an error: %v", err)
	}
	if summary.Total != 0 || len(summary.ByCategory) != 0 || len(summary.Sessions) != 0 {
		t.Fatalf("unknown agent summary = %+v, want all zeroes", summary)
	}
	if summary.OldestAt != nil {
		t.Fatalf("unknown agent oldest_at = %v, want absent", summary.OldestAt)
	}
}

// TestSummaryScopeDenied pins §7.5: another agent's summary without operator
// scope is refused with NUDGE_SCOPE_DENIED.
func TestSummaryScopeDenied(t *testing.T) {
	f := newNudgeFixture(t, 2)
	if _, err := f.summary(t, `{"agent_id":"bravo"}`); err == nil {
		t.Fatal("another agent's summary must be refused")
	} else if !strings.Contains(err.Error(), "NUDGE_SCOPE_DENIED") {
		t.Fatalf("error = %v, want NUDGE_SCOPE_DENIED", err)
	}

	// The SAME call succeeds once operator scope is armed.
	f.server.operatorScope = true
	if _, err := f.summary(t, `{"agent_id":"bravo"}`); err != nil {
		t.Fatalf("operator scope must permit the read: %v", err)
	}
}

// TestSummaryAgentRequired pins NUDGE_AGENT_REQUIRED: neither an explicit agent
// nor a bridge identity means there is nobody to answer for.
func TestSummaryAgentRequired(t *testing.T) {
	f := newNudgeFixture(t, 1)
	f.server.agentID = ""
	if _, err := f.summary(t, `{}`); err == nil {
		t.Fatal("a summary with no agent identity must be refused")
	} else if !strings.Contains(err.Error(), "NUDGE_AGENT_REQUIRED") {
		t.Fatalf("error = %v, want NUDGE_AGENT_REQUIRED", err)
	}
}

// TestUnreadListBounded pins §7.6: the limit defaults to 20 and clamps at 100,
// and the list carries the bodies (this is the explicit read).
func TestUnreadListBounded(t *testing.T) {
	cases := map[int]int{
		0:    NudgeDefaultListLimit,
		-5:   NudgeDefaultListLimit,
		37:   37,
		100:  100,
		101:  NudgeMaxListLimit,
		5000: NudgeMaxListLimit,
	}
	for in, want := range cases {
		if got := ClampUnreadLimit(in); got != want {
			t.Fatalf("ClampUnreadLimit(%d) = %d, want %d", in, got, want)
		}
	}

	f := newNudgeFixture(t, 30)
	body, err := f.server.handleCourierUnreadList(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	list, ok := body.(UnreadList)
	if !ok {
		t.Fatalf("list body is %T", body)
	}
	if list.Count != NudgeDefaultListLimit || len(list.Messages) != NudgeDefaultListLimit {
		t.Fatalf("default limit must return %d of 30: count=%d len=%d",
			NudgeDefaultListLimit, list.Count, len(list.Messages))
	}
	if !strings.Contains(list.Messages[0].Text, nudgeSecret) {
		t.Fatalf("the explicit list tool is where bodies arrive: %+v", list.Messages[0])
	}
	if list.Messages[0].SessionID != f.sessionID || list.Messages[0].Author != nudgeOther {
		t.Fatalf("list row must carry session and author: %+v", list.Messages[0])
	}
	if list.Messages[0].Seq >= list.Messages[1].Seq {
		t.Fatalf("list must be seq-ordered: %+v", list.Messages[:2])
	}

	// Over the maximum clamps to 100 rather than erroring; 30 available means
	// 30 rows on the wire.
	body, err = f.server.handleCourierUnreadList(json.RawMessage(`{"limit":5000}`))
	if err != nil {
		t.Fatalf("over-max limit must clamp, not error: %v", err)
	}
	list = body.(UnreadList)
	if list.Count != 30 {
		t.Fatalf("all 30 unread must be returned under the clamp: %d", list.Count)
	}

	// An explicit small limit is honoured exactly.
	body, err = f.server.handleCourierUnreadList(json.RawMessage(`{"limit":5}`))
	if err != nil {
		t.Fatalf("limit 5: %v", err)
	}
	if got := body.(UnreadList).Count; got != 5 {
		t.Fatalf("limit 5 returned %d", got)
	}
}

// TestUnreadListIncludesTagsAndNeverAdvanceCursor proves the list is a READ:
// tags come back, and the cursor is untouched after both tools ran.
func TestUnreadListIncludesTags(t *testing.T) {
	f := newNudgeFixture(t, 3)
	body, err := f.server.handleCourierUnreadList(json.RawMessage(`{"limit":100}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	list := body.(UnreadList)
	if len(list.Messages) != 3 {
		t.Fatalf("want 3 unread session messages, got %d", len(list.Messages))
	}
	if len(list.Messages[0].Tags) != 1 || list.Messages[0].Tags[0] != "build" {
		t.Fatalf("tags must ride the list: %+v", list.Messages[0])
	}
	if list.Messages[1].Tags == nil {
		t.Fatal("an untagged message must report an empty array, never null")
	}

	// Reading again must return the same rows: nothing was advanced.
	again, err := f.server.handleCourierUnreadList(json.RawMessage(`{"limit":100}`))
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if got := again.(UnreadList).Count; got != len(list.Messages) {
		t.Fatalf("the list must not mark anything read: first=%d second=%d", len(list.Messages), got)
	}
}

// TestNudgeResourceAndPrompt pins the other two §2.1 surfaces: the resource
// serves the same counts-only JSON, and the prompt renders the one-line
// template from the live counts.
func TestNudgeResourceAndPrompt(t *testing.T) {
	f := newNudgeFixture(t, 3)

	read, err := f.server.handleResourcesRead(json.RawMessage(`{"uri":"courier://inbox/alpha/unread"}`))
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	contents := read.(map[string]any)["contents"].([]resourceContent)
	if len(contents) != 1 || contents[0].MimeType != "application/json" {
		t.Fatalf("resource contents = %+v", contents)
	}
	if strings.Contains(contents[0].Text, nudgeSecret) || strings.Contains(contents[0].Text, nudgeInbox) {
		t.Fatalf("the resource leaked a body: %s", contents[0].Text)
	}
	if !strings.Contains(contents[0].Text, `"total"`) {
		t.Fatalf("the resource must serve the summary JSON: %s", contents[0].Text)
	}

	prompt, err := f.server.handlePromptsGet(json.RawMessage(`{"name":"courier_nudge","arguments":{}}`))
	if err != nil {
		t.Fatalf("prompts/get: %v", err)
	}
	rendered := fmt.Sprintf("%v", prompt)
	if !strings.Contains(rendered, "You have 5 unread messages in courier") {
		t.Fatalf("prompt must render the live count: %v", prompt)
	}
	if !strings.Contains(rendered, "Call courier_unread_list to read them.") {
		t.Fatalf("prompt must name the read tool: %v", prompt)
	}

	// A zero-count world renders the zero sentence, not "0 unread ()".
	if got := NudgePromptTemplate(0, nil); got != nudgePromptZero {
		t.Fatalf("zero prompt = %q", got)
	}
}

// TestNudgeDisabledRemovesSurface pins mcp.nudge.enabled=false: the tool,
// resource and prompt are gone and nothing else moved.
func TestNudgeDisabledRemovesSurface(t *testing.T) {
	s := NewWithOptions(registry.NewMemoryStore(), Options{NudgeDisabled: true, AgentID: "alpha"})
	if s.nudgeEnabled() {
		t.Fatal("NudgeDisabled must disarm the surface")
	}
	for _, def := range s.toolDefinitions() {
		if strings.HasPrefix(def.Name, "courier_") {
			t.Fatalf("disabled nudge still advertises %q", def.Name)
		}
	}
	if _, ok := s.tools["courier_unread_summary"]; ok {
		t.Fatal("disabled nudge still registers the summary handler")
	}
	if got := s.handlePromptsList().(map[string]any)["prompts"].([]promptDefinition); len(got) != 0 {
		t.Fatalf("disabled nudge still lists prompts: %+v", got)
	}
	if got := s.handleResourcesList().(map[string]any)["resources"].([]resourceEntry); len(got) != 0 {
		t.Fatalf("disabled nudge still lists resources: %+v", got)
	}
	// The harness tools are independent of the nudge switch.
	if _, ok := s.tools["harness_list"]; !ok {
		t.Fatal("harness_list must not follow the nudge switch")
	}
}

// TestSessionStartCountsMatchSummary pins the join between the two halves of
// the feature: the session-start prefix is fed by the SAME summary the tool
// answers, so a harness is never told a different number than an agent reads.
func TestSessionStartCountsMatchSummary(t *testing.T) {
	f := newNudgeFixture(t, 3)
	counts := SessionStartCounts(f.server.courier, nudgeAgent)
	if counts == nil {
		t.Fatal("a wired courier must produce a counts hook")
	}
	total, categories := counts(context.Background(), harnessMessage(f.sessionID))
	if total != 5 {
		t.Fatalf("hook total = %d, want 5", total)
	}
	// Busiest first: uncategorized (3), then build/urgent (1 each, alphabetical).
	want := []string{Uncategorized, "build", "urgent"}
	if strings.Join(categories, ",") != strings.Join(want, ",") {
		t.Fatalf("hook categories = %v, want %v", categories, want)
	}
	if SessionStartCounts(nil, nudgeAgent) != nil {
		t.Fatal("no courier source must mean no hook")
	}
}

// harnessMessage is the delivery-shape message the counts hook is called with.
func harnessMessage(sessionID string) harness.Message {
	return harness.Message{ID: "m", SessionID: sessionID, Text: "body"}
}

// TestHarnessTools pins the two carried-over tools (COURIER-HARNESS §4.4):
// harness_list reports an empty surface when none is wired, and harness_probe
// names the unknown adapter without inventing one.
func TestHarnessTools(t *testing.T) {
	s := New(registry.NewMemoryStore())
	body, err := s.handleHarnessList(nil)
	if err != nil {
		t.Fatalf("harness_list: %v", err)
	}
	if got := body.(HarnessListOutput); got.Count != 0 || len(got.Harnesses) != 0 {
		t.Fatalf("an unwired registry must list no adapters: %+v", got)
	}
	if _, err := s.handleHarnessProbe(json.RawMessage(`{"name":"hermes"}`)); err == nil {
		t.Fatal("probing an unknown adapter must error")
	} else if !strings.Contains(err.Error(), "HARNESS_UNKNOWN") {
		t.Fatalf("error = %v, want HARNESS_UNKNOWN", err)
	}
	if _, err := s.handleHarnessProbe(json.RawMessage(`{}`)); err == nil {
		t.Fatal("a probe with no name must be refused")
	}
}
