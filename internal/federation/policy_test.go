package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// CR-CHAT-023 — per-peer policy: record semantics, file loading, the outbound
// namespace wall on forwardPass, and the peer-identity announcement header.
// ---------------------------------------------------------------------------

// policyRelay is a fake linked relay recording the last forwarded request.
type policyRelay struct {
	status  int
	body    string
	gotPath string
	gotBody []byte
	gotHop  string
	gotPeer string
	calls   int
}

func (r *policyRelay) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.calls++
		r.gotPath = req.URL.Path
		r.gotHop = req.Header.Get(HopHeader)
		r.gotPeer = req.Header.Get(PeerHeader)
		buf := make([]byte, req.ContentLength)
		if req.ContentLength > 0 {
			_, _ = req.Body.Read(buf)
		}
		r.gotBody = buf
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		_, _ = w.Write([]byte(r.body))
	}
}

func TestPeerPolicyAdmitsNamespaceDefaultDeny(t *testing.T) {
	var nilPolicy *PeerPolicy
	if nilPolicy.AdmitsNamespace("anything") {
		t.Fatal("a nil policy must admit nothing")
	}
	p := &PeerPolicy{Peer: "peer_acme"}
	if p.AdmitsNamespace("") || p.AdmitsNamespace("acme-mirror") {
		t.Fatal("default is deny: an empty NamespacesAllow admits no namespace")
	}
	p.NamespacesAllow = []string{"acme-mirror"}
	if !p.AdmitsNamespace("acme-mirror") {
		t.Fatal("the listed namespace must be admitted")
	}
	if p.AdmitsNamespace("other") {
		t.Fatal("a namespace outside the allow list must be refused")
	}
}

func TestPeerPolicyAdmitsAgentDenyWinsOverAllow(t *testing.T) {
	p := &PeerPolicy{Peer: "p"}
	// Empty allow admits everything the namespace gate already admitted.
	if !p.AdmitsAgent("atlas") || !p.AdmitsAgent("deploy-bot") {
		t.Fatal("an empty agents.allow admits every agent not denied")
	}
	p.AgentsDeny = []string{"deploy-bot"}
	if p.AdmitsAgent("deploy-bot") {
		t.Fatal("deny must refuse even with an empty allow")
	}
	p.AgentsAllow = []string{"atlas"}
	if !p.AdmitsAgent("atlas") {
		t.Fatal("a listed agent must be admitted")
	}
	if p.AdmitsAgent("quill") {
		t.Fatal("an agent outside a non-empty allow list must be refused")
	}
	// Deny wins over allow (§6.1).
	p.AgentsAllow = append(p.AgentsAllow, "deploy-bot")
	if p.AdmitsAgent("deploy-bot") {
		t.Fatal("deny wins over allow: an explicit exclusion is never overridden")
	}
}

func TestLoadPeerPoliciesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")
	if err := os.WriteFile(path, []byte(`{"peers":[
		{"peer":"peer_acme","url":"http://127.0.0.1:9","namespaces_allow":["acme-mirror"],"agents_deny":["deploy-bot"]},
		{"peer":"peer_beta","namespaces_allow":["beta-lab","acme-mirror"]}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pp, err := LoadPeerPoliciesFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(pp) != 2 {
		t.Fatalf("policies = %d, want 2", len(pp))
	}
	if pp.PolicyFor("peer_acme") == nil || pp.PolicyFor("peer_beta") == nil {
		t.Fatal("both peers must resolve")
	}
	if pp.PolicyFor("peer_ghost") != nil {
		t.Fatal("an unknown peer must not resolve a policy")
	}

	// Empty peer id is refused.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"peers":[{"peer":"  "}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPeerPoliciesFile(bad); err == nil {
		t.Fatal("a record with no peer id must be a load error")
	}
	// Duplicate ids are refused.
	dup := filepath.Join(dir, "dup.json")
	if err := os.WriteFile(dup, []byte(`{"peers":[{"peer":"a"},{"peer":"a"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPeerPoliciesFile(dup); err == nil {
		t.Fatal("duplicate peer ids must be a load error")
	}
}

// outboundWall checks the source-side wall: a delivery whose namespace is not
// admitted by the link's peer policy never leaves, and never becomes a
// transient error or a 404.
func TestForwardPassPeerPolicyNamespaceWall(t *testing.T) {
	remote := &policyRelay{status: http.StatusOK, body: `{"id":"x"}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, 0, "")
	client.SetPeerPolicies(PeerPolicies{
		"peer_beta": {Peer: "peer_beta", URL: srv.URL, SelfAs: "self_acme",
			NamespacesAllow: []string{"beta-lab"}},
	})

	// (a) Admitted namespace: the forward goes out, with the peer header.
	status, _, err := client.ForwardToAny(context.Background(), "atlas",
		[]byte(`{"payload":{"a":1},"namespace":"beta-lab"}`))
	if err != nil {
		t.Fatalf("admitted namespace: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if remote.gotHop != "1" {
		t.Fatalf("hop header = %q, want 1", remote.gotHop)
	}
	if remote.gotPeer != "self_acme" {
		t.Fatalf("peer header = %q, want self_acme", remote.gotPeer)
	}
	if remote.gotPath != "/agents/atlas/inbox" {
		t.Fatalf("path = %q, want the shipped inbox endpoint", remote.gotPath)
	}
	remote.calls = 0

	// (b) Refused namespace: nothing is forwarded, and the answer is the
	// definitive FED_NAMESPACE_NOT_PERMITTED — not transient, not 404.
	_, _, err = client.ForwardToAny(context.Background(), "atlas",
		[]byte(`{"payload":{"a":1},"namespace":"elsewhere"}`))
	if err == nil {
		t.Fatal("refused namespace: expected an error")
	}
	var nsnp *NamespaceNotPermittedError
	if !errors.As(err, &nsnp) {
		t.Fatalf("err = %T (%v), want *NamespaceNotPermittedError", err, err)
	}
	if remote.calls != 0 {
		t.Fatalf("remote calls = %d, want 0: a policy-blocked delivery must not leave", remote.calls)
	}

	// (c) Body with no namespace member: the implicit default realm, equally
	// subject to the wall.
	_, _, err = client.ForwardToAny(context.Background(), "atlas", []byte(`{"payload":{}}`))
	if err == nil {
		t.Fatal("no-namespace body: expected the wall to refuse (default deny)")
	}
	if remote.calls != 0 {
		t.Fatalf("remote calls = %d, want 0", remote.calls)
	}

	// (d) No policy on the link: shipped behavior unchanged.
	client2 := NewClient([]string{srv.URL}, 0, "")
	if status, _, err := client2.ForwardToAny(context.Background(), "atlas", []byte(`{"payload":{}}`)); err != nil || status != http.StatusOK {
		t.Fatalf("no-policy link: status %d err %v, want shipped 200/nil", status, err)
	}
}

func TestForwardPassPolicyBlockedPlusReachableLink(t *testing.T) {
	// Two links: the first is policy-blocked, the second reachable. The
	// forward must ride the second; a policy block is not an outage.
	blocked := &policyRelay{status: http.StatusOK, body: `{"id":"x"}`}
	blockedSrv := httptest.NewServer(blocked.handler())
	defer blockedSrv.Close()
	open := &policyRelay{status: http.StatusOK, body: `{"id":"y"}`}
	openSrv := httptest.NewServer(open.handler())
	defer openSrv.Close()

	client := NewClient([]string{blockedSrv.URL, openSrv.URL}, 0, "")
	client.SetPeerPolicies(PeerPolicies{
		"peer_x": {Peer: "peer_x", URL: blockedSrv.URL, NamespacesAllow: []string{"beta-lab"}},
	})
	status, _, err := client.ForwardToAny(context.Background(), "atlas",
		[]byte(`{"payload":{},"namespace":"elsewhere"}`))
	if err != nil {
		t.Fatalf("reachable second link: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the second link", status)
	}
	if blocked.calls != 0 {
		t.Fatalf("blocked link calls = %d, want 0", blocked.calls)
	}
	if open.calls != 1 {
		t.Fatalf("open link calls = %d, want 1", open.calls)
	}
}

func TestForwardPassPolicyBlockIsNotTransient(t *testing.T) {
	// A policy-blocked link must NOT be classified transient: when all links
	// are policy-blocked the answer is the definitive namespace refusal, so
	// the caller never holds a delivery a policy refuses.
	remote := &policyRelay{status: http.StatusOK, body: `{}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, 0, "")
	client.SetPeerPolicies(PeerPolicies{
		"p": {Peer: "p", URL: srv.URL, NamespacesAllow: []string{"allowed"}},
	})
	_, _, err := client.ForwardToAny(context.Background(), "atlas", []byte(`{"payload":{},"namespace":"denied"}`))
	if err == nil {
		t.Fatal("expected the namespace refusal")
	}
	var transient *TransientError
	if errors.As(err, &transient) {
		t.Fatalf("a policy block must never read as transient (%v)", err)
	}
}

func TestForwardDeliverNoPolicyNoHeader(t *testing.T) {
	remote := &policyRelay{status: http.StatusOK, body: `{}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()
	client := NewClient([]string{srv.URL}, 0, "")
	if _, _, err := client.ForwardToAny(context.Background(), "a", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if remote.gotPeer != "" {
		t.Fatalf("peer header = %q, want none without a policy", remote.gotPeer)
	}
	var body map[string]any
	if err := json.Unmarshal(remote.gotBody, &body); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
}
