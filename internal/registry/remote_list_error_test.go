package registry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// listErrReporter mirrors the optional ListErrorReporter Store capability
// (store.go) locally. Asserting through a local structural type keeps this
// file's RED run at the pre-fix revision BEHAVIORAL (ok=false) instead of a
// package-wide collection error, so the captured failure names the defect.
type listErrReporter interface {
	ListError() error
}

// TestRemoteStore_ListReportsServerError pins the DF-CRIER-199 contract for
// a failing backend: GET /agents answering 500 must still yield a NON-NIL
// empty slice (the Store contract the spec pins), but the store must also
// expose the failure — with the HTTP status and the response body — so a
// failing server is distinguishable from an empty registry. Pre-fix,
// RemoteStore laundered every failure into a silent empty slice.
func TestRemoteStore_ListReportsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry backend exploded", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	rs := NewRemoteStore(srv.URL, "bridge-client", "")

	got := rs.List()
	if got == nil {
		t.Fatal("List() = nil slice, want non-nil empty slice per the Store contract")
	}
	if len(got) != 0 {
		t.Fatalf("List() = %d agents, want 0 on failure", len(got))
	}

	rep, ok := any(rs).(listErrReporter)
	if !ok {
		t.Fatal("RemoteStore does not implement ListErrorReporter — a failing List is indistinguishable from an empty registry")
	}
	err := rep.ListError()
	if err == nil {
		t.Fatal("ListError() = nil after a failed List, want the recorded failure")
	}
	for _, want := range []string{"500", "registry backend exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ListError() = %q, want it to carry %q", err.Error(), want)
		}
	}
}

// TestRemoteStore_ListReportsDecodeError covers the second laundering
// class: a 200 whose body the response envelope cannot decode. The Store
// contract answer is unchanged (non-nil empty slice) but the recorded
// error must name the decode step.
func TestRemoteStore_ListReportsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("this-is-not-json"))
	}))
	t.Cleanup(srv.Close)

	rs := NewRemoteStore(srv.URL, "bridge-client", "")

	got := rs.List()
	if got == nil || len(got) != 0 {
		t.Fatalf("List() = %+v, want non-nil empty slice on decode failure", got)
	}

	rep, ok := any(rs).(listErrReporter)
	if !ok {
		t.Fatal("RemoteStore does not implement ListErrorReporter")
	}
	err := rep.ListError()
	if err == nil {
		t.Fatal("ListError() = nil after an undecodable response, want the recorded failure")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("ListError() = %q, want it to name the decode step", err.Error())
	}
}

// TestRemoteStore_ListEmptyRegistryIsClean pins the other side of the
// distinction: a HEALTHY server answering {"agents":[]} (a genuinely empty
// registry) yields the same non-nil empty slice but a NIL ListError — and a
// store that failed once clears the recorded error on the next successful
// List, so ListError always describes the LAST call.
func TestRemoteStore_ListEmptyRegistryIsClean(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "transient failure", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agents":[]}`))
	}))
	t.Cleanup(srv.Close)

	rs := NewRemoteStore(srv.URL, "bridge-client", "")
	rep, ok := any(rs).(listErrReporter)
	if !ok {
		t.Fatal("RemoteStore does not implement ListErrorReporter")
	}

	// First call fails (502): recorded error, empty slice.
	if got := rs.List(); got == nil || len(got) != 0 {
		t.Fatalf("List() on 502 = %+v, want non-nil empty slice", got)
	}
	if err := rep.ListError(); err == nil {
		t.Fatal("ListError() = nil after the 502 call, want the recorded failure")
	}

	// Second call succeeds with an empty registry: error cleared, empty slice.
	got := rs.List()
	if got == nil {
		t.Fatal("List() = nil slice on success, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("List() = %d agents, want 0 for an empty registry", len(got))
	}
	if err := rep.ListError(); err != nil {
		t.Fatalf("ListError() = %v after a successful empty list, want nil", err)
	}
}
