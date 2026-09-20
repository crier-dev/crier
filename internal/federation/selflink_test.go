package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// DF-CRIER-12: a CR_FED_LINKS entry that addresses THIS relay is not a remote
// peer. GET /fed/peers already carries the local relay as its first entry, so
// listing such a link again reported the relay twice — once under its display
// name and once under the address the operator typed.
//
// The skip cannot be a URL (or host:port) string comparison: the local entry is
// rendered http://localhost:<port> regardless of what the operator configured,
// while the link may say 127.0.0.1:<port>. Both reach the same listener, so the
// comparison normalises the host (loopback spellings are equivalent) and uses
// the port — which the relay serves on every interface — as the discriminator.

// selfLinkPort is the port used by the fixtures below; it is never dialled,
// because a self-link is skipped before any fetch.
const selfLinkPort = 8899

func selfLinkURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", selfLinkPort)
}

func localPeerFixture() Peer {
	return Peer{
		Name:   "relay-a",
		URL:    fmt.Sprintf("http://localhost:%d", selfLinkPort),
		Agents: []RemoteAgent{{ID: "local-agent-a", Capabilities: []string{"code"}}},
	}
}

// TestPeersSkipsSelfLink covers the client-level contract: with only a
// self-addressing link configured, the peer list is empty (nothing is fetched,
// nothing is emitted).
func TestPeersSkipsSelfLink(t *testing.T) {
	c := NewClient([]string{selfLinkURL()}, 0, "")
	c.SetSelf(selfLinkPort)

	peers := c.Peers(context.Background())
	if len(peers) != 0 {
		t.Fatalf("Peers() with only a self-link = %+v, want no peers (the link addresses this relay)", peers)
	}
}

// TestHandlePeersSelfLinkListsLocalOnce is the reported case:
// CR_FED_NAME=relay-a + CR_FED_LINKS=http://127.0.0.1:<own port> must yield
// exactly ONE peer — the local entry, first, with its live agents.
func TestHandlePeersSelfLinkListsLocalOnce(t *testing.T) {
	c := NewClient([]string{selfLinkURL()}, 0, "")
	c.SetSelf(selfLinkPort)

	h := HandlePeers(c, localPeerFixture)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/fed/peers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Peers []Peer `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Peers) != 1 {
		t.Fatalf("peers len = %d, want 1 (the local relay exactly once): %s", len(out.Peers), rec.Body.String())
	}
	if out.Peers[0].Name != "relay-a" {
		t.Errorf("peers[0].name = %q, want the local entry first (relay-a)", out.Peers[0].Name)
	}
	if len(out.Peers[0].Agents) != 1 || out.Peers[0].Agents[0].ID != "local-agent-a" {
		t.Errorf("local agents = %+v, want the live local registry list", out.Peers[0].Agents)
	}
}

// TestHandlePeersSelfLinkAlongsideRemote proves the skip is targeted: with a
// self-link AND a genuine remote link configured, the listing is the local
// entry (first) plus exactly one remote peer carrying its live agents, and the
// wire shape stays {"peers":[{name,url,agents}]} — no extra fields.
func TestHandlePeersSelfLinkAlongsideRemote(t *testing.T) {
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"id":"remote-a","capabilities":["x"]}]}`))
	}))
	defer remoteSrv.Close()

	c := NewClient([]string{selfLinkURL(), remoteSrv.URL}, 0, "")
	c.SetSelf(selfLinkPort)

	h := HandlePeers(c, localPeerFixture)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/fed/peers", nil))

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
		t.Fatalf("peers len = %d, want 2 (local + the one genuine remote): %s", len(out.Peers), rec.Body.String())
	}
	if out.Peers[0].Name != "relay-a" {
		t.Errorf("peers[0] = %+v, want the local entry first", out.Peers[0])
	}
	if out.Peers[1].URL != remoteSrv.URL {
		t.Errorf("peers[1].url = %q, want the remote link %q", out.Peers[1].URL, remoteSrv.URL)
	}
	if len(out.Peers[1].Agents) != 1 || out.Peers[1].Agents[0].ID != "remote-a" {
		t.Errorf("remote agents = %+v, want [remote-a] fetched live over the link", out.Peers[1].Agents)
	}

	// Wire shape: exactly name/url/agents per peer, under a top-level "peers".
	var raw struct {
		Peers []map[string]json.RawMessage `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	if len(raw.Peers) != 2 {
		t.Fatalf("raw peers len = %d, want 2", len(raw.Peers))
	}
	for i, p := range raw.Peers {
		if len(p) != 3 {
			t.Errorf("peers[%d] has %d fields, want exactly name/url/agents: %v", i, len(p), p)
		}
		for _, key := range []string{"name", "url", "agents"} {
			if _, ok := p[key]; !ok {
				t.Errorf("peers[%d] missing %q (the documented wire shape)", i, key)
			}
		}
	}
}

// TestIsSelfLink is table-driven over the boundary cases: which CR_FED_LINKS
// spellings address this relay, and which are genuine remotes.
func TestIsSelfLink(t *testing.T) {
	tests := []struct {
		name     string
		selfPort int
		link     string
		want     bool
	}{
		{"own port via 127.0.0.1", selfLinkPort, selfLinkURL(), true},
		{"own port via localhost", selfLinkPort, fmt.Sprintf("http://localhost:%d", selfLinkPort), true},
		{"own port via IPv6 loopback", selfLinkPort, fmt.Sprintf("http://[::1]:%d", selfLinkPort), true},
		{"own port via undefined literal", selfLinkPort, fmt.Sprintf("http://0.0.0.0:%d", selfLinkPort), true},
		{"own port over https", selfLinkPort, fmt.Sprintf("https://localhost:%d", selfLinkPort), true},
		{"own port via uppercase LOCALHOST", selfLinkPort, fmt.Sprintf("http://LOCALHOST:%d", selfLinkPort), true},
		{"own port with a trailing slash", selfLinkPort, selfLinkURL() + "/", true},
		{"loopback but a different port", selfLinkPort, fmt.Sprintf("http://127.0.0.1:%d", selfLinkPort-1), false},
		{"own port on a LAN hostname", selfLinkPort, fmt.Sprintf("http://relay-b.lan:%d", selfLinkPort), false},
		{"own port on a public IP", selfLinkPort, fmt.Sprintf("http://203.0.113.7:%d", selfLinkPort), false},
		{"remote LAN link", selfLinkPort, fmt.Sprintf("http://192.168.123.102:%d", selfLinkPort), false},
		{"identity unknown (SetSelf never called)", 0, selfLinkURL(), false},
		{"loopback link with no port is not our listener", selfLinkPort, "http://127.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient([]string{tt.link}, 0, "")
			c.SetSelf(tt.selfPort)
			if len(c.Links()) != 1 {
				t.Fatalf("parseLink rejected %q — the fixture is not a valid link", tt.link)
			}
			if got := c.isSelfLink(c.Links()[0]); got != tt.want {
				t.Errorf("isSelfLink(%q) with self port %d = %v, want %v", tt.link, tt.selfPort, got, tt.want)
			}
			if got := len(c.SelfLinks()); (got == 1) != tt.want {
				t.Errorf("SelfLinks() len = %d, want %v for %q", got, tt.want, tt.link)
			}
		})
	}
}

// TestLinkPortDefaults pins the scheme-default resolution used by isSelfLink:
// a link written without an explicit port addresses the scheme default, so it
// is not this relay's port unless the two coincide.
func TestLinkPortDefaults(t *testing.T) {
	tests := []struct {
		link     string
		wantPort int
		wantOK   bool
	}{
		{"http://relay-b.example.com", 80, true},
		{"https://relay-b.example.com", 443, true},
		{"http://relay-b.example.com:18772", 18772, true},
	}
	for _, tt := range tests {
		t.Run(tt.link, func(t *testing.T) {
			u, err := url.Parse(tt.link)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tt.link, err)
			}
			got, ok := linkPort(u)
			if ok != tt.wantOK || got != tt.wantPort {
				t.Errorf("linkPort(%q) = (%d, %v), want (%d, %v)", tt.link, got, ok, tt.wantPort, tt.wantOK)
			}
		})
	}

	// An unresolvable port has no listener it can name, so linkPort reports
	// "unknown" rather than guessing. Measured: url.Parse rejects a
	// non-numeric port outright (so parseLink drops it as config garbage —
	// TestParseLinkRejectsNonNumericPort), while an out-of-range numeric port
	// parses and is what reaches linkPort's lookup failure here.
	t.Run("unresolvable port (out of range)", func(t *testing.T) {
		u, err := url.Parse("http://relay-b.example.com:999999")
		if err != nil {
			t.Fatalf("url.Parse: %v", err)
		}
		if u.Port() != "999999" {
			t.Fatalf("Port() = %q, want the parsed out-of-range port to reach linkPort", u.Port())
		}
		got, ok := linkPort(u)
		if ok || got != 0 {
			t.Errorf("linkPort with an unresolvable port = (%d, %v), want (0, false)", got, ok)
		}
	})
}

// TestParseLinkRejectsNonNumericPort pins that a malformed port in
// CR_FED_LINKS is dropped as config garbage (so it can never be misclassified
// as this relay's own address), while a well-formed self-link survives parsing
// and is recognised by isSelfLink.
func TestParseLinkRejectsNonNumericPort(t *testing.T) {
	if _, ok := parseLink("http://relay-b.example.com:notaport"); ok {
		t.Error("parseLink accepted a non-numeric port; want it skipped as config garbage")
	}
	l, ok := parseLink(selfLinkURL())
	if !ok {
		t.Fatalf("parseLink rejected the self-link spelling %q", selfLinkURL())
	}
	c := NewClient([]string{selfLinkURL()}, 0, "")
	c.SetSelf(selfLinkPort)
	if !c.isSelfLink(l) {
		t.Errorf("isSelfLink(%+v) = false, want true for this relay's own address", l)
	}
}

// TestStandardLinkStillListed proves the ordinary case is untouched: a client
// that never declares a local identity lists every configured link exactly as
// before — which is why the pre-existing TestPeers / TestHandlePeers keep
// passing unchanged.
func TestStandardLinkStillListed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"id":"remote-a","capabilities":[]}]}`))
	}))
	defer srv.Close()

	c := NewClient([]string{srv.URL}, 0, "")
	peers := c.Peers(context.Background())
	if len(peers) != 1 || peers[0].URL != srv.URL {
		t.Fatalf("Peers() = %+v, want the single remote link listed", peers)
	}
	if len(peers[0].Agents) != 1 || peers[0].Agents[0].ID != "remote-a" {
		t.Errorf("agents = %+v, want [remote-a]", peers[0].Agents)
	}
	if self := c.SelfLinks(); len(self) != 0 {
		t.Errorf("SelfLinks() = %+v, want none when SetSelf was never called", self)
	}
}
