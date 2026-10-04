package daggerctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// recordedRequest is one request the fake dagger surface received.
type recordedRequest struct {
	Method string
	Path   string
	Body   string
	Auth   string
}

// fakeDagger is a stand-in for the EXISTING dagger surface: it answers the
// documented HTTP/JSON contract and records what it was asked. The bridge is
// proven against it, so the tests never need a real executor — which is the
// boundary rule (D18) made executable.
type fakeDagger struct {
	mu       sync.Mutex
	requests []recordedRequest

	// handler, when set, answers instead of the default OK view.
	handler func(w http.ResponseWriter, r *http.Request, body string) bool
}

func (f *fakeDagger) record(r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   body,
		Auth:   r.Header.Get("Authorization"),
	})
}

func (f *fakeDagger) Requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// ServeHTTP answers the contract with a run view whose run_id is derived from
// the path, so a test can tell which call produced it.
func (f *fakeDagger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.record(r, string(body))
	if f.handler != nil && f.handler(w, r, string(body)) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_, _ = w.Write([]byte(`{"run_id":"run-from-get","status":"running"}`))
	default:
		_, _ = w.Write([]byte(`{"run_id":"run-from-post","status":"running"}`))
	}
}

func newFakeDagger(t *testing.T) (*fakeDagger, string) {
	t.Helper()
	f := &fakeDagger{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// TestHTTPBridgeSpeaksTheDocumentedContract pins every request the bridge makes
// — method, path and body — so the adapter cannot drift from the contract the
// README documents.
func TestHTTPBridgeSpeaksTheDocumentedContract(t *testing.T) {
	fake, url := newFakeDagger(t)
	bridge, err := daggerctl.NewHTTPBridge(url)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	ctx := context.Background()

	if _, err := bridge.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "build"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := bridge.RunStatus(ctx, "run-1"); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if _, err := bridge.Cancel(ctx, "run-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := bridge.Resume(ctx, "run-1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := bridge.Rewind(ctx, "run-1", "node-3"); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if _, err := bridge.RunSkill(ctx, daggerctl.RunSkillRequest{Skill: "summarize", Args: map[string]any{"lang": "es"}}); err != nil {
		t.Fatalf("RunSkill: %v", err)
	}

	want := []recordedRequest{
		{Method: http.MethodPost, Path: "/execute", Body: `{"prompt":"build"}`},
		{Method: http.MethodGet, Path: "/runs/run-1"},
		{Method: http.MethodPost, Path: "/runs/run-1/cancel"},
		{Method: http.MethodPost, Path: "/runs/run-1/resume"},
		{Method: http.MethodPost, Path: "/runs/run-1/rewind", Body: `{"node_id":"node-3"}`},
		{Method: http.MethodPost, Path: "/skills/summarize/run", Body: `{"args":{"lang":"es"}}`},
	}
	got := fake.Requests()
	if len(got) != len(want) {
		t.Fatalf("bridge made %d request(s), want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Method != w.Method || got[i].Path != w.Path {
			t.Errorf("request %d = %s %s, want %s %s", i, got[i].Method, got[i].Path, w.Method, w.Path)
		}
		if w.Body != "" {
			if !sameJSON(t, got[i].Body, w.Body) {
				t.Errorf("request %d body = %s, want %s", i, got[i].Body, w.Body)
			}
		} else if strings.TrimSpace(got[i].Body) != "" {
			t.Errorf("request %d carried an unexpected body: %s", i, got[i].Body)
		}
	}
}

func TestHTTPBridgeSendsTheBearerTokenWhenConfigured(t *testing.T) {
	fake, url := newFakeDagger(t)
	bridge, err := daggerctl.NewHTTPBridge(url, daggerctl.WithBearerToken("s3cret"))
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	if _, err := bridge.RunStatus(context.Background(), "run-1"); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	got := fake.Requests()
	if len(got) != 1 || got[0].Auth != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want %q", got[0].Auth, "Bearer s3cret")
	}
}

func TestHTTPBridgeAcceptsTheStateAlias(t *testing.T) {
	fake := &fakeDagger{handler: func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"run-1","state":"succeeded"}`))
		return true
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	bridge, err := daggerctl.NewHTTPBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	view, err := bridge.RunStatus(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if view.State != daggerctl.StateSucceeded {
		t.Errorf("state = %q, want succeeded (from the `state` alias)", view.State)
	}
}

func TestHTTPBridgeTranslates404ToRunNotFound(t *testing.T) {
	fake := &fakeDagger{handler: func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		http.Error(w, `{"error":"no such run"}`, http.StatusNotFound)
		return true
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	bridge, err := daggerctl.NewHTTPBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	_, err = bridge.RunStatus(context.Background(), "missing")
	if !errors.Is(err, daggerctl.ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestHTTPBridgeReportsNon2xxAsABridgeErrorNamingTheDetail(t *testing.T) {
	fake := &fakeDagger{handler: func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"executor exploded"}`))
		return true
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	bridge, err := daggerctl.NewHTTPBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	_, err = bridge.Cancel(context.Background(), "run-1")
	if !errors.Is(err, daggerctl.ErrBridge) {
		t.Fatalf("err = %v, want ErrBridge", err)
	}
	if !strings.Contains(err.Error(), "executor exploded") || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %q, want it to name the status and the executor's detail", err)
	}
}

func TestHTTPBridgeRefusesAViewWithNoRunID(t *testing.T) {
	fake := &fakeDagger{handler: func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running"}`))
		return true
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	bridge, err := daggerctl.NewHTTPBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	if _, err := bridge.CreateRun(context.Background(), daggerctl.CreateRunRequest{Prompt: "p"}); !errors.Is(err, daggerctl.ErrBridge) {
		t.Fatalf("err = %v, want ErrBridge (a run crier cannot name is a run it cannot hold)", err)
	}
}

func TestNewHTTPBridgeRefusesAnEmptyOrRelativeURL(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-url", "/just/a/path"} {
		if _, err := daggerctl.NewHTTPBridge(bad); err == nil {
			t.Errorf("NewHTTPBridge(%q) accepted a URL it cannot dial", bad)
		}
	}
}

// TestHTTPBridgeIsClientOnly asserts the bridge touches nothing but the network
// surface it was given: the answering server here expresses a complete exchange
// (create then observe) with no other process involved.
func TestHTTPBridgeIsClientOnly(t *testing.T) {
	fake, url := newFakeDagger(t)
	bridge, err := daggerctl.NewHTTPBridge(url)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	if got := bridge.BaseURL(); got != url {
		t.Errorf("BaseURL = %q, want %q", got, url)
	}
	if _, err := bridge.CreateRun(context.Background(), daggerctl.CreateRunRequest{Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(fake.Requests()) != 1 {
		t.Errorf("a bridge call must be exactly one request to the configured endpoint, got %d", len(fake.Requests()))
	}
}

// sameJSON compares two JSON documents by value.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return jsonEqual(av, bv)
}

func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ab) == string(bb)
}
