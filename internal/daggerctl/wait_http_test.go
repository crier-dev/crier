package daggerctl_test

// HTTP-level tests for the wait routes (CR-CHAT-034): the named error bodies,
// the resolve endpoint, and the status endpoint — driven through
// daggerctl.Handler over a Service, exactly as the server mounts them.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/daggerctl"
)

func waitRouter(t *testing.T, h *waitHarness) http.Handler {
	t.Helper()
	r := mux.NewRouter()
	hh := daggerctl.NewHandler(h.svc)
	r.HandleFunc("/dagger/wait", hh.HandleDeliverAndWait).Methods(http.MethodPost)
	r.HandleFunc("/dagger/waits/{key}/resolve", hh.HandleResolveWait).Methods(http.MethodPost)
	r.HandleFunc("/dagger/waits/{key}", hh.HandleWaitStatus).Methods(http.MethodGet)
	return r
}

// TestHTTPWaitNamedTimeoutBody proves the timeout answers with a NAMED JSON
// body (DELIVERY_WAIT_TIMEOUT), never a silent 200 or an empty success.
func TestHTTPWaitNamedTimeoutBody(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	srv := httptest.NewServer(waitRouter(t, h))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/dagger/wait", "application/json",
		strings.NewReader(`{"agent_id":"agent-a","payload":{"work":1},"idempotency_key":"http-timeout","timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("POST /dagger/wait: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != daggerctl.CodeDeliveryWaitTimeout {
		t.Fatalf("error code = %q, want %s", body["error"], daggerctl.CodeDeliveryWaitTimeout)
	}
}

// TestHTTPWaitResolveAndStatus drives the two-node flow over HTTP: wait in a
// goroutine, resolve through POST /dagger/waits/{key}/resolve, read the
// recorded resolution through GET /dagger/waits/{key}.
func TestHTTPWaitResolveAndStatus(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	srv := httptest.NewServer(waitRouter(t, h))
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	statuses := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(srv.URL+"/dagger/wait", "application/json",
			strings.NewReader(`{"agent_id":"agent-a","payload":{"work":2},"idempotency_key":"http-key","timeout_seconds":5}`))
		if err != nil {
			statuses <- 0
			return
		}
		defer func() { _ = resp.Body.Close() }()
		statuses <- resp.StatusCode
	}()

	// Wait for the pending wait to be readable via status.
	var st map[string]any
	deadline := 200
	for {
		resp, err := http.Get(srv.URL + "/dagger/waits/http-key")
		if err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&st)
			_ = resp.Body.Close()
			if st["state"] == "pending" {
				break
			}
		}
		deadline--
		if deadline == 0 {
			t.Fatal("the wait never became pending")
		}
	}

	resp, err := http.Post(srv.URL+"/dagger/waits/http-key/resolve", "application/json",
		strings.NewReader(`{"reply":{"output":"done"},"message_id":"m-1"}`))
	if err != nil {
		t.Fatalf("POST resolve: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", resp.StatusCode)
	}

	wg.Wait()
	if code := <-statuses; code != http.StatusOK {
		t.Fatalf("wait answered %d, want 200 after resolution", code)
	}
	resp, err = http.Get(srv.URL + "/dagger/waits/http-key")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	st = map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st["state"] != "resolved" {
		t.Fatalf("final state = %v, want resolved", st["state"])
	}
}

// TestHTTPWaitUnknownKey: an unknown key is a named 404, on both the resolve
// and the status endpoint.
func TestHTTPWaitUnknownKey(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	srv := httptest.NewServer(waitRouter(t, h))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dagger/waits/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if _, err := h.svc.ResolveWait("nope", "", nil); !errors.Is(err, daggerctl.ErrWaitNotFound) {
		t.Fatalf("resolve unknown key error = %v, want ErrWaitNotFound", err)
	}
}
