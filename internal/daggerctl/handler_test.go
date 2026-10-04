package daggerctl_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// newControlRouter builds the exact route set cmd/server registers, so a test
// exercises the handler through a real router (path variables included) rather
// than calling the handler directly with a hand-built request.
func newControlRouter(svc *daggerctl.Service) *mux.Router {
	h := daggerctl.NewHandler(svc)
	r := mux.NewRouter()
	r.HandleFunc("/dagger/runs", h.HandleCreateRun).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}", h.HandleGetRun).Methods(http.MethodGet)
	r.HandleFunc("/dagger/runs/{id}/cancel", h.HandleCancel).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/resume", h.HandleResume).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/rewind", h.HandleRewind).Methods(http.MethodPost)
	r.HandleFunc("/dagger/skills/{skill}/run", h.HandleRunSkill).Methods(http.MethodPost)
	return r
}

func do(t *testing.T, router *mux.Router, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestControlRoutesServeEveryVerb(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := daggerctl.NewService(bridge, daggerctl.NewMemoryStore(), &recordingDeliverer{})
	router := newControlRouter(svc)

	rec, out := do(t, router, http.MethodPost, "/dagger/runs", `{"agent_id":"hermes-1","prompt":"build"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /dagger/runs = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	if out["run_id"] != "run-1" || out["requesting_agent"] != "hermes-1" {
		t.Fatalf("create body = %v", out)
	}

	rec, out = do(t, router, http.MethodGet, "/dagger/runs/run-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dagger/runs/run-1 = %d, want 200", rec.Code)
	}
	if out["state"] != string(daggerctl.StateRunning) {
		t.Fatalf("status body = %v", out)
	}

	for _, verb := range []string{"cancel", "resume"} {
		rec, _ = do(t, router, http.MethodPost, "/dagger/runs/run-1/"+verb, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /dagger/runs/run-1/%s = %d, want 200 (body %s)", verb, rec.Code, rec.Body)
		}
	}

	rec, _ = do(t, router, http.MethodPost, "/dagger/runs/run-1/rewind", `{"node_id":"node-2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rewind = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	rec, out = do(t, router, http.MethodPost, "/dagger/skills/summarize/run", `{"agent_id":"hermes-1","args":{"lang":"es"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /dagger/skills/summarize/run = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	if out["skill"] != "summarize" {
		t.Fatalf("skill body = %v", out)
	}
}

func TestControlRoutesAnswerNamedErrors(t *testing.T) {
	t.Run("missing run is 404", func(t *testing.T) {
		svc := daggerctl.NewService(&fakeBridge{}, daggerctl.NewMemoryStore(), &recordingDeliverer{})
		router := newControlRouter(svc)
		rec, out := do(t, router, http.MethodGet, "/dagger/runs/absent", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if !strings.Contains(out["error"].(string), "dagger run not found") {
			t.Errorf("body = %v, want the named refusal", out)
		}
	})

	t.Run("invalid input is 400", func(t *testing.T) {
		svc := daggerctl.NewService(&fakeBridge{}, daggerctl.NewMemoryStore(), &recordingDeliverer{})
		router := newControlRouter(svc)
		rec, _ := do(t, router, http.MethodPost, "/dagger/runs", `{"agent_id":"a"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown body member is refused", func(t *testing.T) {
		svc := daggerctl.NewService(&fakeBridge{}, daggerctl.NewMemoryStore(), &recordingDeliverer{})
		router := newControlRouter(svc)
		rec, out := do(t, router, http.MethodPost, "/dagger/runs", `{"agent_id":"a","prompt":"p","bogus":1}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
		}
		if !strings.Contains(out["error"].(string), "bogus") {
			t.Errorf("body = %v, want the offending member named", out)
		}
	})

	t.Run("unconfigured is 503 naming the gap", func(t *testing.T) {
		svc := daggerctl.NewService(nil, daggerctl.NewMemoryStore(), &recordingDeliverer{})
		router := newControlRouter(svc)
		rec, out := do(t, router, http.MethodPost, "/dagger/runs", `{"agent_id":"a","prompt":"p"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body)
		}
		if out["error"] != "DAGGER_UNCONFIGURED" {
			t.Errorf("error code = %v, want DAGGER_UNCONFIGURED", out["error"])
		}
	})

	t.Run("bridge failure is 502", func(t *testing.T) {
		bridge := &fakeBridge{createErr: fmt.Errorf("%w: connection refused", daggerctl.ErrBridge)}
		svc := daggerctl.NewService(bridge, daggerctl.NewMemoryStore(), &recordingDeliverer{})
		router := newControlRouter(svc)
		rec, _ := do(t, router, http.MethodPost, "/dagger/runs", `{"agent_id":"a","prompt":"p"}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body)
		}
	})
}

// TestControlRoutesDeliverTerminalOutcomesThroughTheInboxPath is the route-level
// acceptance: a DAG that finishes is delivered to the requesting agent's durable
// inbox, and the record read back over REST reports it.
func TestControlRoutesDeliverTerminalOutcomesThroughTheInboxPath(t *testing.T) {
	reg := newRegistryWithAgent(t, "hermes-1")
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := daggerctl.NewService(bridge, daggerctl.NewMemoryStore(), daggerctl.InboxDeliverer(reg))
	router := newControlRouter(svc)

	if rec, _ := do(t, router, http.MethodPost, "/dagger/runs", `{"agent_id":"hermes-1","prompt":"deploy"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	bridge.status = func(runID string) (*daggerctl.RunView, error) {
		return &daggerctl.RunView{RunID: runID, State: daggerctl.StateSucceeded}, nil
	}
	rec, out := do(t, router, http.MethodGet, "/dagger/runs/run-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if out["notified"] != true || out["state"] != string(daggerctl.StateSucceeded) {
		t.Fatalf("record = %v, want succeeded + notified", out)
	}
	assertInboxHoldsNotification(t, reg, "hermes-1", daggerctl.CodeRunSucceeded)
}
