package daggerctl_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// TestRESTClientRoundTripsThroughTheControlRoutes drives the REST client against
// the SAME router cmd/server registers, over a real HTTP server: the two halves
// of the surface are proven to agree, and a terminal outcome still lands in the
// requesting agent's inbox.
func TestRESTClientRoundTripsThroughTheControlRoutes(t *testing.T) {
	reg := newRegistryWithAgent(t, "hermes-1")
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := daggerctl.NewService(bridge, daggerctl.NewMemoryStore(), daggerctl.InboxDeliverer(reg))

	srv := httptest.NewServer(newControlRouter(svc))
	t.Cleanup(srv.Close)
	client := daggerctl.NewRESTClient(srv.URL, "")
	if client == nil {
		t.Fatal("NewRESTClient returned nil for a non-empty base URL")
	}
	ctx := context.Background()

	rec, err := client.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "hermes-1", Prompt: "deploy"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if rec.RunID != "run-1" || rec.RequestingAgent != "hermes-1" {
		t.Fatalf("create returned %+v", rec)
	}

	if _, err := client.RunStatus(ctx, "run-1"); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if _, err := client.Resume(ctx, "run-1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := client.Rewind(ctx, "run-1", "node-2"); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if _, err := client.RunSkill(ctx, daggerctl.RunSkillRequest{AgentID: "hermes-1", Skill: "summarize"}); err != nil {
		t.Fatalf("RunSkill: %v", err)
	}

	// Terminal transition observed through the client, delivered through the
	// shipped inbox path.
	bridge.status = func(runID string) (*daggerctl.RunView, error) {
		return &daggerctl.RunView{RunID: runID, State: daggerctl.StateSucceeded}, nil
	}
	rec, err = client.RunStatus(ctx, "run-1")
	if err != nil {
		t.Fatalf("RunStatus(terminal): %v", err)
	}
	if rec.State != daggerctl.StateSucceeded || !rec.Notified {
		t.Fatalf("terminal record = %+v", rec)
	}
	assertInboxHoldsNotification(t, reg, "hermes-1", daggerctl.CodeRunSucceeded)
}

// TestRESTClientUsesTheCancelRoute pins cancel separately: its record must come
// back with the cancelled state.
func TestRESTClientUsesTheCancelRoute(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := daggerctl.NewService(bridge, daggerctl.NewMemoryStore(), &recordingDeliverer{})
	srv := httptest.NewServer(newControlRouter(svc))
	t.Cleanup(srv.Close)
	client := daggerctl.NewRESTClient(srv.URL, "")

	ctx := context.Background()
	if _, err := client.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec, err := client.Cancel(ctx, "run-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if rec.State != daggerctl.StateCancelled {
		t.Fatalf("state = %q, want cancelled", rec.State)
	}
	if got := bridge.Calls(); !contains(got, "cancel:run-1") {
		t.Errorf("bridge calls = %v", got)
	}
}

// TestRESTClientPinsTheCrierRouteShapes records what the client sends, so the
// paths it uses cannot drift from the routes the server registers.
func TestRESTClientPinsTheCrierRouteShapes(t *testing.T) {
	type hit struct{ method, path, body string }
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf []byte
		if r.Body != nil {
			buf, _ = io.ReadAll(r.Body)
		}
		hits = append(hits, hit{method: r.Method, path: r.URL.Path, body: string(buf)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"run-1","state":"running","requesting_agent":"a","kind":"prompt"}`))
	}))
	t.Cleanup(srv.Close)
	client := daggerctl.NewRESTClient(srv.URL, "tok")

	ctx := context.Background()
	if _, err := client.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := client.RunStatus(ctx, "run-1"); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}

	want := []hit{
		{method: http.MethodPost, path: "/dagger/runs"},
		{method: http.MethodGet, path: "/dagger/runs/run-1"},
	}
	if len(hits) != len(want) {
		t.Fatalf("client made %d request(s), want %d: %+v", len(hits), len(want), hits)
	}
	for i, w := range want {
		if hits[i].method != w.method || hits[i].path != w.path {
			t.Errorf("request %d = %s %s, want %s %s", i, hits[i].method, hits[i].path, w.method, w.path)
		}
	}
	if !strings.Contains(hits[0].body, `"prompt":"p"`) || !strings.Contains(hits[0].body, `"agent_id":"a"`) {
		t.Errorf("create body = %s", hits[0].body)
	}
}

func TestRESTClientTranslatesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/absent") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"dagger run not found"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"DAGGER_UNCONFIGURED"}`))
	}))
	t.Cleanup(srv.Close)
	client := daggerctl.NewRESTClient(srv.URL, "")
	ctx := context.Background()

	if _, err := client.RunStatus(ctx, "absent"); !errors.Is(err, daggerctl.ErrRunNotFound) {
		t.Fatalf("404 -> %v, want ErrRunNotFound", err)
	}
	if _, err := client.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); !errors.Is(err, daggerctl.ErrBridge) {
		t.Fatalf("503 -> %v, want ErrBridge", err)
	}
}

func TestNewRESTClientWithNoBaseURLIsNil(t *testing.T) {
	if c := daggerctl.NewRESTClient("   ", "tok"); c != nil {
		t.Fatalf("NewRESTClient(\"\") = %v, want nil", c)
	}
}
