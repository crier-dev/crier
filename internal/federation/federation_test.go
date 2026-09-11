package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLink(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantURL string
		wantNam string
	}{
		{"plain http", "http://localhost:18772", true, "http://localhost:18772", "localhost:18772"},
		{"trailing slash trimmed", "http://localhost:18772/", true, "http://localhost:18772", "localhost:18772"},
		{"https", "https://relay.example.com:443", true, "https://relay.example.com:443", "relay.example.com:443"},
		{"surrounding whitespace", "  http://localhost:18772  ", true, "http://localhost:18772", "localhost:18772"},
		{"empty", "", false, "", ""},
		{"no scheme", "localhost:18772", false, "", ""},
		{"garbage", "not a url", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLink(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("parseLink(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.URL != tt.wantURL || got.Name != tt.wantNam {
				t.Errorf("parseLink(%q) = {%q, %q}, want {%q, %q}",
					tt.raw, got.URL, got.Name, tt.wantURL, tt.wantNam)
			}
		})
	}
}

func TestNewClientSkipsInvalidLinks(t *testing.T) {
	c := NewClient([]string{"http://localhost:1", "garbage", "", "https://localhost:2"}, 0, "")
	links := c.Links()
	if len(links) != 2 {
		t.Fatalf("Links() len = %d, want 2 (invalid entries skipped)", len(links))
	}
	if links[0].URL != "http://localhost:1" || links[1].URL != "https://localhost:2" {
		t.Errorf("unexpected links: %+v", links)
	}
}

// remoteRelay is a fake linked relay: it records the last deliver request
// and answers with the given status/body.
type remoteRelay struct {
	t         *testing.T
	gotMethod string
	gotPath   string
	gotBody   []byte
	gotHop    string
	gotAuth   string
	status    int
	body      string
}

func (r *remoteRelay) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.gotMethod = req.Method
		r.gotPath = req.URL.Path
		r.gotHop = req.Header.Get(HopHeader)
		r.gotAuth = req.Header.Get("Authorization")
		body := make([]byte, req.ContentLength)
		if req.ContentLength > 0 {
			if _, err := req.Body.Read(body); err != nil && err.Error() != "EOF" {
				r.t.Fatalf("read body: %v", err)
			}
		}
		r.gotBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		w.Write([]byte(r.body))
	}
}

func TestForwardDeliver(t *testing.T) {
	rr := &remoteRelay{t: t, status: http.StatusOK, body: `{"id":"abc","reply":{"text":"hi"}}`}
	srv := httptest.NewServer(rr.handler())
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, "")
	body := []byte(`{"payload":{"text":"hello"},"sender":"agent-1","delivery_mode":"blocking"}`)
	status, respBody, err := c.ForwardDeliver(context.Background(), c.Links()[0], "agent-2", body)
	if err != nil {
		t.Fatalf("ForwardDeliver: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if string(respBody) != `{"id":"abc","reply":{"text":"hi"}}` {
		t.Errorf("body = %s, want relayed verbatim", respBody)
	}
	if rr.gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", rr.gotMethod)
	}
	if rr.gotPath != "/agents/agent-2/inbox" {
		t.Errorf("path = %s, want /agents/agent-2/inbox", rr.gotPath)
	}
	if string(rr.gotBody) != string(body) {
		t.Errorf("forwarded body = %s, want the original deliver JSON", rr.gotBody)
	}
	if rr.gotHop != "1" {
		t.Errorf("hop header = %q, want \"1\" (loop prevention)", rr.gotHop)
	}
	if rr.gotAuth != "" {
		t.Errorf("Authorization header = %q, want none (CR_FED_TOKEN unset)", rr.gotAuth)
	}
}

// TestForwardDeliverLinkAuth proves the DF-CRIER-6 contract: a client built
// with the CR_FED_TOKEN shared secret sends the exact
// "Authorization: Bearer <token>" header on forwarded delivers, while body
// passthrough and the hop header stay untouched.
func TestForwardDeliverLinkAuth(t *testing.T) {
	const token = "shared-federation-secret"
	rr := &remoteRelay{t: t, status: http.StatusOK, body: `{"id":"ok"}`}
	srv := httptest.NewServer(rr.handler())
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, token)
	body := []byte(`{"payload":{"text":"hello"},"sender":"agent-1","delivery_mode":"blocking"}`)
	status, respBody, err := c.ForwardDeliver(context.Background(), c.Links()[0], "agent-2", body)
	if err != nil {
		t.Fatalf("ForwardDeliver: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if rr.gotAuth != "Bearer "+token {
		t.Errorf("Authorization header = %q, want exactly %q", rr.gotAuth, "Bearer "+token)
	}
	// Auth must not disturb the existing forwarding contract.
	if string(rr.gotBody) != string(body) {
		t.Errorf("forwarded body = %s, want the original deliver JSON", rr.gotBody)
	}
	if rr.gotHop != "1" {
		t.Errorf("hop header = %q, want \"1\" (loop prevention)", rr.gotHop)
	}
	if string(respBody) != `{"id":"ok"}` {
		t.Errorf("response body = %s, want relayed verbatim", respBody)
	}
}

// TestFetchRemoteAgentsLinkAuth proves the discovery request also carries
// the Bearer header when the token is set (GET /agents is protected by the
// same CR_AUTH_TOKEN middleware on the remote relay).
func TestFetchRemoteAgentsLinkAuth(t *testing.T) {
	const token = "shared-federation-secret"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[]}`))
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, token)
	if _, err := c.FetchRemoteAgents(context.Background(), c.Links()[0]); err != nil {
		t.Fatalf("FetchRemoteAgents: %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization header = %q, want exactly %q", gotAuth, "Bearer "+token)
	}
}

// TestPeersDoesNotExposeToken proves the shared secret never surfaces in
// the /fed/peers listing (it is a secret, not link metadata).
func TestPeersDoesNotExposeToken(t *testing.T) {
	const token = "shared-federation-secret-leak-probe"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"id":"remote-a","capabilities":[]}]}`))
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, token)
	h := HandlePeers(c, func() Peer {
		return Peer{Name: "local", URL: "http://localhost:18771", Agents: []RemoteAgent{}}
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/fed/peers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, token) {
		t.Errorf("/fed/peers response contains the shared secret: %s", body)
	}
}

func TestForwardToAnySkipsNotFoundAndWinsOnFirstHit(t *testing.T) {
	rr1 := &remoteRelay{t: t, status: http.StatusNotFound, body: `{"error":"agent not found"}`}
	rr2 := &remoteRelay{t: t, status: http.StatusOK, body: `{"id":"remote-1"}`}
	srv1 := httptest.NewServer(rr1.handler())
	srv2 := httptest.NewServer(rr2.handler())
	defer srv1.Close()
	defer srv2.Close()

	c := NewClient([]string{srv1.URL, srv2.URL}, 0, "")
	status, body, err := c.ForwardToAny(context.Background(), "agent-remote", []byte(`{"payload":{}}`))
	if err != nil {
		t.Fatalf("ForwardToAny: %v", err)
	}
	if status != http.StatusOK || string(body) != `{"id":"remote-1"}` {
		t.Errorf("got (%d, %s), want (200, {\"id\":\"remote-1\"})", status, body)
	}
	// The 404 relay must have been tried first.
	if rr1.gotPath == "" || rr2.gotPath == "" {
		t.Error("both relays must receive the forward attempt")
	}
}

func TestForwardToAnyAllNotFound(t *testing.T) {
	rr1 := &remoteRelay{t: t, status: http.StatusNotFound, body: `{"error":"agent not found"}`}
	rr2 := &remoteRelay{t: t, status: http.StatusNotFound, body: `{"error":"agent not found"}`}
	srv1 := httptest.NewServer(rr1.handler())
	srv2 := httptest.NewServer(rr2.handler())
	defer srv1.Close()
	defer srv2.Close()

	c := NewClient([]string{srv1.URL, srv2.URL}, 0, "")
	if _, _, err := c.ForwardToAny(context.Background(), "ghost", []byte(`{"payload":{}}`)); err == nil {
		t.Fatal("ForwardToAny all-404: want error, got nil")
	}
}

func TestForwardToAnyUnreachableLinkFallsThrough(t *testing.T) {
	// First link: a server that is closed immediately (unreachable).
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()

	rr2 := &remoteRelay{t: t, status: http.StatusAccepted, body: `{"id":"ok-2"}`}
	srv2 := httptest.NewServer(rr2.handler())
	defer srv2.Close()

	c := NewClient([]string{dead.URL, srv2.URL}, 0, "")
	status, body, err := c.ForwardToAny(context.Background(), "agent-remote", []byte(`{"payload":{}}`))
	if err != nil {
		t.Fatalf("ForwardToAny: %v", err)
	}
	if status != http.StatusAccepted || string(body) != `{"id":"ok-2"}` {
		t.Errorf("got (%d, %s), want (202, {\"id\":\"ok-2\"})", status, body)
	}
}

func TestForwardToAnyNoLinks(t *testing.T) {
	c := NewClient(nil, 0, "")
	if _, _, err := c.ForwardToAny(context.Background(), "ghost", []byte(`{}`)); err == nil {
		t.Fatal("ForwardToAny with no links: want error, got nil")
	}
}

func TestFetchRemoteAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents" {
			t.Errorf("path = %s, want /agents", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[
			{"id":"a-1","capabilities":["chat","code"],"public_key":"deadbeef"},
			{"id":"a-2","capabilities":[],"status":"online"}
		]}`))
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, "")
	agents, err := c.FetchRemoteAgents(context.Background(), c.Links()[0])
	if err != nil {
		t.Fatalf("FetchRemoteAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents len = %d, want 2", len(agents))
	}
	if agents[0].ID != "a-1" || len(agents[0].Capabilities) != 2 {
		t.Errorf("agents[0] = %+v, want a-1 with 2 capabilities", agents[0])
	}
	if agents[1].ID != "a-2" || agents[1].Capabilities == nil {
		t.Errorf("agents[1] = %+v, want a-2 with empty (non-nil) capabilities", agents[1])
	}
}

func TestFetchRemoteAgentsNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, "")
	if _, err := c.FetchRemoteAgents(context.Background(), c.Links()[0]); err == nil {
		t.Fatal("FetchRemoteAgents on 500: want error, got nil")
	}
}

func TestPeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"id":"remote-a","capabilities":["x"]}]}`))
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, "")
	peers := c.Peers(context.Background())
	if len(peers) != 1 {
		t.Fatalf("peers len = %d, want 1", len(peers))
	}
	p := peers[0]
	if p.Name != strings.TrimPrefix(strings.TrimPrefix(srv.URL, "http://"), "https://") {
		t.Errorf("peer name = %q, want host:port derived from link URL", p.Name)
	}
	if len(p.Agents) != 1 || p.Agents[0].ID != "remote-a" {
		t.Errorf("peer agents = %+v, want [remote-a]", p.Agents)
	}
}

func TestPeersUnreachableLinkStillListed(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()

	c := NewClient([]string{dead.URL}, 0, "")
	peers := c.Peers(context.Background())
	if len(peers) != 1 {
		t.Fatalf("peers len = %d, want 1 (down link still listed)", len(peers))
	}
	if peers[0].Agents == nil || len(peers[0].Agents) != 0 {
		t.Errorf("down-link agents = %+v, want empty list", peers[0].Agents)
	}
}

func TestHandlePeers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"id":"remote-a","capabilities":["x"]}]}`))
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, "")
	h := HandlePeers(c, func() Peer {
		return Peer{Name: "local", URL: "http://localhost:18771", Agents: []RemoteAgent{{ID: "local-a", Capabilities: []string{"y"}}}}
	})

	req := httptest.NewRequest(http.MethodGet, "/fed/peers", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Peers []Peer `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Peers) != 2 {
		t.Fatalf("peers len = %d, want 2 (local + linked)", len(out.Peers))
	}
	// Local relay first, linked relay second with its remote agents.
	if out.Peers[0].Name != "local" || out.Peers[0].URL != "http://localhost:18771" {
		t.Errorf("local peer = %+v, want {local, http://localhost:18771}", out.Peers[0])
	}
	if len(out.Peers[0].Agents) != 1 || out.Peers[0].Agents[0].ID != "local-a" {
		t.Errorf("local agents = %+v, want [local-a]", out.Peers[0].Agents)
	}
	if len(out.Peers[1].Agents) != 1 || out.Peers[1].Agents[0].ID != "remote-a" {
		t.Errorf("remote agents = %+v, want [remote-a]", out.Peers[1].Agents)
	}
}

func TestHandlePeersNoClient(t *testing.T) {
	h := HandlePeers(nil, func() Peer {
		return Peer{Name: "solo", URL: "http://localhost:1", Agents: []RemoteAgent{}}
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/fed/peers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Peers []Peer `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Peers) != 1 || out.Peers[0].Name != "solo" {
		t.Errorf("peers = %+v, want just the local entry", out.Peers)
	}
}
