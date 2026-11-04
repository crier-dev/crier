package guard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// MON-CRIER-003 unit half: the accessors and counters that make the guard's
// fail-open behaviour visible. The cmd/server half proves the same values
// reach GET /metrics from a real boot.

// scrapeDefault reads the process-wide registry through its real HTTP
// handler — byte-for-byte what GET /metrics serves when the surface is on.
func scrapeDefault(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(metrics.Default.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape default registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read exposition: %v", err)
	}
	return string(b)
}

// bareSample returns the value of an unlabelled "name value" sample, or -1
// when the series is absent (which is itself a failure: the counter is
// registered at package init).
func bareSample(body, name string) float64 {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(line, name+" %g", &v); err == nil {
			return v
		}
	}
	return -1
}

// TestRouter_InflightAccessor proves the in-flight gauge reads the real
// semaphore: 0 at rest, 1 while a check holds it, 0 again after release.
func TestRouter_InflightAccessor(t *testing.T) {
	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	defer release() // never leave httptest.Server.Close waiting on the handler

	h := &countingHandler{response: verdictAllowJSON, block: block}
	srv := httptest.NewServer(h)
	defer srv.Close()

	r := NewRouter(RouterOptions{
		Timeout:       10 * time.Second,
		MaxConcurrent: 2,
		LookupEnv:     envMap{"K": "k"}.get,
	})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	if got := r.Inflight(); got != 0 {
		t.Fatalf("Inflight at rest = %d, want 0", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = r.Check(context.Background(), p, "s", "u")
	}()

	deadline := time.Now().Add(5 * time.Second)
	for r.Inflight() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("Inflight never reached 1 while a check was blocked in the provider call (got %d)", r.Inflight())
		}
		time.Sleep(5 * time.Millisecond)
	}

	release()
	<-done
	if got := r.Inflight(); got != 0 {
		t.Fatalf("Inflight after the check returned = %d, want 0", got)
	}
}

// TestRouter_OpenProvidersFlipsAfterThreshold is the circuit-open half: the
// provider label flips to open after CR_GUARD_CIRCUIT_THRESHOLD consecutive
// failures and back to closed once a probe succeeds — the same breaker the
// router already routes on, read through the exported accessor.
func TestRouter_OpenProvidersFlipsAfterThreshold(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"message":"mock"}}`))
			return
		}
		_, _ = w.Write([]byte(verdictAllowJSON))
	}))
	defer srv.Close()

	r := NewRouter(RouterOptions{
		Timeout:          2 * time.Second,
		CircuitThreshold: 1,
		CircuitCooldown:  50 * time.Millisecond,
		LookupEnv:        envMap{"K": "k"}.get,
	})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})

	// Before any check every bounded provider is present and closed.
	open := r.OpenProviders()
	for _, name := range ProviderNames() {
		value, ok := open[name]
		if !ok {
			t.Fatalf("OpenProviders missing bounded provider %q (map: %v)", name, open)
		}
		if value {
			t.Fatalf("provider %q reported open before any check", name)
		}
	}

	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
		t.Fatal("Check against a 500 provider returned nil error")
	}
	open = r.OpenProviders()
	if !open["custom"] {
		t.Fatalf("custom circuit not reported open after reaching the threshold: %v", open)
	}
	if open["deepseek"] {
		t.Fatalf("deepseek circuit reported open although only the custom endpoint failed: %v", open)
	}

	// Healthy probe after the cooldown closes the circuit again.
	status.Store(http.StatusOK)
	time.Sleep(80 * time.Millisecond)
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err != nil {
		t.Fatalf("probe after cooldown should succeed: %v", err)
	}
	if open := r.OpenProviders(); open["custom"] {
		t.Fatalf("custom circuit still reported open after a successful probe: %v", open)
	}
}

// TestCircuit_IsOpenAccessor pins the exported read-only view and proves it
// does not change breaker semantics: it flips exactly at the threshold and
// is reset by a success.
func TestCircuit_IsOpenAccessor(t *testing.T) {
	c := NewCircuit(2, time.Minute)
	if c.IsOpen("k") {
		t.Fatal("fresh circuit reported open")
	}
	c.recordFailure("k")
	if c.IsOpen("k") {
		t.Fatal("circuit opened below the threshold")
	}
	c.recordFailure("k")
	if !c.IsOpen("k") {
		t.Fatal("circuit not open at the threshold")
	}
	if c.IsOpen("other") {
		t.Fatal("IsOpen leaked another key's state")
	}
	c.recordSuccess("k")
	if c.IsOpen("k") {
		t.Fatal("circuit still open after recordSuccess")
	}
}

// TestProviderNames_Bounded pins the label domain: the preset enum plus
// "custom", sorted, no duplicates — nothing that could grow per request.
func TestProviderNames_Bounded(t *testing.T) {
	names := ProviderNames()
	if got, want := len(names), len(Presets)+1; got != want {
		t.Fatalf("ProviderNames = %v (%d values), want %d (presets + custom)", names, got, want)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("ProviderNames not sorted: %v", names)
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("duplicate provider label %q in %v", name, names)
		}
		seen[name] = true
	}
	for preset := range Presets {
		if !seen[preset] {
			t.Fatalf("preset %q missing from the label domain %v", preset, names)
		}
	}
	if !seen["custom"] {
		t.Fatalf("\"custom\" missing from the label domain %v", names)
	}
}

// TestGuard_NilAccessors proves the disabled-guard posture is a zero reading,
// not a panic: the composition layer passes a nil *Guard and the gauges still
// evaluate.
func TestGuard_NilAccessors(t *testing.T) {
	var g *Guard
	if got := g.Inflight(); got != 0 {
		t.Fatalf("nil guard Inflight = %d, want 0", got)
	}
	if got := g.OpenProviders(); len(got) != 0 {
		t.Fatalf("nil guard OpenProviders = %v, want empty", got)
	}
}

// TestRouter_FailOpenCounters proves the two counters advance exactly at the
// fail-open returns they name: a saturated semaphore, and a chain that could
// not even attempt a call (no provider api key).
func TestRouter_FailOpenCounters(t *testing.T) {
	before := scrapeDefault(t)
	satBefore := bareSample(before, "crier_guard_saturated_total")
	failBefore := bareSample(before, "crier_guard_all_providers_failed_total")
	if satBefore < 0 || failBefore < 0 {
		t.Fatalf("fail-open counters absent from the exposition (saturated=%v failed=%v)", satBefore, failBefore)
	}

	// No key anywhere: the chain never attempts a call → ErrAllProvidersFailed
	// on the no-api-key arm.
	r := NewRouter(RouterOptions{Timeout: 2 * time.Second, LookupEnv: envMap{}.get})
	p := policyWith(ProviderSpec{Provider: "custom", BaseURL: "http://127.0.0.1:1", APIKeyRef: "env:NOPE", Model: "m"})
	if _, _, _, err := r.Check(context.Background(), p, "s", "u"); err == nil {
		t.Fatal("Check with no key returned nil error")
	}

	// Saturation: one check holds the only slot, a second waits out the 2s
	// acquire budget and fails open.
	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	defer release()

	srv := httptest.NewServer(&countingHandler{response: verdictAllowJSON, block: block})
	defer srv.Close()

	satRouter := NewRouter(RouterOptions{
		Timeout:       10 * time.Second,
		MaxConcurrent: 1,
		LookupEnv:     envMap{"K": "k"}.get,
	})
	satPolicy := policyWith(ProviderSpec{Provider: "custom", BaseURL: srv.URL, APIKeyRef: "env:K", Model: "m"})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _, _, _ = satRouter.Check(context.Background(), satPolicy, "s", "u")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for satRouter.Inflight() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("first check never took the semaphore (Inflight=%d)", satRouter.Inflight())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, _, err := satRouter.Check(context.Background(), satPolicy, "s", "u"); err == nil {
		t.Fatal("second check over a full semaphore returned nil error")
	}
	release()
	<-firstDone

	after := scrapeDefault(t)
	satAfter := bareSample(after, "crier_guard_saturated_total")
	failAfter := bareSample(after, "crier_guard_all_providers_failed_total")
	if !(satAfter > satBefore) {
		t.Errorf("crier_guard_saturated_total did not advance: before=%v after=%v", satBefore, satAfter)
	}
	if !(failAfter > failBefore) {
		t.Errorf("crier_guard_all_providers_failed_total did not advance: before=%v after=%v", failBefore, failAfter)
	}
}
