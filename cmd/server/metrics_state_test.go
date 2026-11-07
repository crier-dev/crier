// metrics_state_test.go — MON-CRIER-004 acceptance evidence (observability-spec
// §3.3, rows G5 / G7 / G8): the series that report STATE rather than traffic.
//
//   - crier_webhook_queue_depth / crier_webhook_queue_oldest_age_seconds
//   - crier_webhook_attempts_total{attempt}
//   - crier_federation_held_age_seconds
//   - crier_store_queue_depth_read_errors_total{backend}
//   - crier_detect_alerts_total{signal,severity}
//   - crier_detect_quarantined_current
//
// Three kinds of proof, in this order:
//
//   - a live boot (metrics on, in-memory store) renders every series — at 0 when
//     idle, never absent, never NaN;
//   - an unreachable webhook endpoint makes the backlog depth and the oldest age
//     ADVANCE on a live boot (the G5 gap: today only a log line said so);
//   - the store depth read-error counter advances once per failed scrape, with
//     the bounded backend label, while the documented NaN reading is unchanged.
package main

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// stateSeriesNames is the seven-series contract this row adds. Every name must
// be present (with a sample) on a boot that has the subsystem, and must render
// 0 — not NaN — when the object behind it is nil.
var stateSeriesNames = []string{
	"crier_webhook_queue_depth",
	"crier_webhook_queue_oldest_age_seconds",
	"crier_webhook_attempts_total",
	"crier_federation_held_age_seconds",
	"crier_store_queue_depth_read_errors_total",
	"crier_detect_alerts_total",
	"crier_detect_quarantined_current",
}

// bootStateMetricsServer boots the real server with metrics on and the in-memory
// registry backend (the bootObservabilityServer harness: auth off, free port,
// run-scoped cleanup).
func bootStateMetricsServer(t *testing.T) string {
	t.Helper()
	t.Setenv("CR_ENABLE_METRICS", "true")
	t.Setenv("CR_ENABLE_PPROF", "")
	// Keyless agent registration and a guard that needs no provider: this row is
	// about the metric surface, not about identity or LLM policy.
	t.Setenv("CR_REQUIRE_AGENT_SIG", "false")
	t.Setenv("CR_GUARD_ENABLED", "false")
	return bootObservabilityServer(t)
}

// scrapeMetrics fetches GET /metrics from a booted server.
func scrapeMetrics(t *testing.T, baseURL string) string {
	t.Helper()
	status, _, body := bpCall(t, baseURL, http.MethodGet, "/metrics", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d, want 200 (body %s)", status, body)
	}
	return body
}

// sampleValue parses the value of the ONE sample whose non-comment line starts
// with prefix, e.g. "crier_detect_quarantined_current" or
// `crier_webhook_attempts_total{attempt="2"}`. ok=false means the line is
// absent — the distinction the assertions need between "missing series" and a
// legitimate 0.
func sampleValue(body, prefix string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		// A prefix naming a bare series must not match a longer name, and a
		// prefix naming a labelled sample must end at the value.
		if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "{") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// TestMetricsState_SeriesRenderOnIdleBoot is the presence half: every series of
// this row renders, at its idle value, on a plain metrics-on boot.
func TestMetricsState_SeriesRenderOnIdleBoot(t *testing.T) {
	baseURL := bootStateMetricsServer(t)
	body := scrapeMetrics(t, baseURL)

	want := []struct {
		prefix string
		value  float64
		why    string
	}{
		{"crier_webhook_queue_depth", 0, "an idle server has no queued redeliveries"},
		{"crier_webhook_queue_oldest_age_seconds", 0, "an empty queue has no oldest item"},
		{"crier_federation_held_age_seconds", 0, "no federation links, so nothing is held"},
		{`crier_store_queue_depth_read_errors_total{backend="memory"}`, 0, "the memory store answers every depth read"},
		{"crier_detect_quarantined_current", 0, "no agent is contained"},
	}
	for _, w := range want {
		got, ok := sampleValue(body, w.prefix)
		if !ok {
			t.Errorf("%s is absent from /metrics — %s\nbody:\n%s", w.prefix, w.why, body)
			continue
		}
		if math.IsNaN(got) {
			t.Errorf("%s = NaN, want %v — a subsystem-state gauge never renders NaN\nbody:\n%s", w.prefix, w.value, body)
			continue
		}
		if got != w.value {
			t.Errorf("%s = %v, want %v — %s", w.prefix, got, w.value, w.why)
		}
	}

	// The webhook ATTEMPT buckets are seeded, not necessarily 0: metrics.Default
	// is process-wide, so a sibling test in this binary that drives a real
	// webhook delivery (MON-CRIER-002's live latency test) legitimately
	// increments the first-attempt ordinal. Presence of every bounded ordinal is
	// this row's contract; "0" only ever held on a pristine process.
	for _, prefix := range []string{
		`crier_webhook_attempts_total{attempt="1"}`,
		`crier_webhook_attempts_total{attempt="6"}`,
	} {
		if got, ok := sampleValue(body, prefix); !ok || math.IsNaN(got) {
			t.Errorf("%s is absent or NaN — every bounded attempt ordinal must be seeded\nbody:\n%s", prefix, body)
		}
	}

	// The detect ALERT pairs are seeded, not necessarily 0 — same mechanism, and
	// the reason the whole suite used to go red between 01:00 and 05:00 UTC
	// (QA-CRIER-56). metrics.Default is process-wide, so an earlier test in this
	// binary that drives real detection traffic during the default quiet window
	// (CR_DETECT_QUIET_HOURS, default 1-5 UTC) legitimately raises
	// odd_hour_volume and leaves the counter at 1 before this boot exists:
	// TestDetectionCatchesAndContainsACompromisedAgent (crfeat030_test.go:152)
	// does exactly that — pairing it with this test reproduces the old red,
	// while this test alone and paired with any other sibling stays green. So
	// the contract asserted here is presence of every bounded (signal,severity)
	// pair, plus a monotonic delta of 0 across a bounded settle window on THIS
	// boot: an idle boot raises no alert of its own. An absolute 0 only ever
	// held on a pristine process.
	detectPairs := []string{
		`crier_detect_alerts_total{signal="canary_trip",severity="critical"}`,
		`crier_detect_alerts_total{signal="odd_hour_volume",severity="warning"}`,
	}
	before := make(map[string]float64, len(detectPairs))
	for _, prefix := range detectPairs {
		got, ok := sampleValue(body, prefix)
		if !ok || math.IsNaN(got) {
			t.Errorf("%s is absent or NaN — every bounded (signal,severity) pair must be seeded\nbody:\n%s", prefix, body)
			continue
		}
		before[prefix] = got
	}
	time.Sleep(250 * time.Millisecond)
	after := scrapeMetrics(t, baseURL)
	for _, prefix := range detectPairs {
		got, ok := sampleValue(after, prefix)
		if !ok || math.IsNaN(got) {
			t.Errorf("%s disappeared from a second scrape of the same boot", prefix)
			continue
		}
		if prev, seen := before[prefix]; seen && got != prev {
			t.Errorf("%s moved from %v to %v on an idle boot — an idle server raises no alert", prefix, prev, got)
		}
	}

	// TYPE lines: these are gauges and counters, spelled the way the spec's
	// catalogue calls them.
	for _, decl := range []string{
		"# TYPE crier_webhook_queue_depth gauge",
		"# TYPE crier_webhook_queue_oldest_age_seconds gauge",
		"# TYPE crier_webhook_attempts_total counter",
		"# TYPE crier_federation_held_age_seconds gauge",
		"# TYPE crier_store_queue_depth_read_errors_total counter",
		"# TYPE crier_detect_alerts_total counter",
		"# TYPE crier_detect_quarantined_current gauge",
	} {
		if !strings.Contains(body, decl) {
			t.Errorf("/metrics is missing %q\nbody:\n%s", decl, body)
		}
	}

	// The federation depth series is namespaced (MON-CRIER-007 closed the
	// dual-emit window) and must stay as registered — this row adds series
	// beside it, it renames nothing.
	if !strings.Contains(body, "# TYPE crier_federation_held_current gauge") {
		t.Error("crier_federation_held_current must stay registered exactly as before")
	}
}

// TestMetricsState_WebhookBacklogAdvancesOnLiveBoot is the G5 half: an endpoint
// that refuses connections makes the queue depth and the oldest-item age go
// non-zero, scraped from the running server. Before this row both facts existed
// only as log lines.
func TestMetricsState_WebhookBacklogAdvancesOnLiveBoot(t *testing.T) {
	baseURL := bootStateMetricsServer(t)

	// A registered agent whose endpoint cannot accept: 127.0.0.1:1 is refused
	// immediately, so the async delivery lands in the retry queue instead of
	// hanging the test on a timeout.
	status, _, raw := bpCall(t, baseURL, http.MethodPost, "/agents",
		`{"id":"m004-hook","webhook":{"url":"http://127.0.0.1:1/nowhere","delivery_mode":"async","retries":5}}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("register webhook agent: status %d, body %s", status, raw)
	}
	// Async webhook delivery is fire-and-forget: 202 Accepted, POSTed by the
	// background drain loop.
	status, _, raw = bpCall(t, baseURL, http.MethodPost, "/agents/m004-hook/inbox",
		`{"sender":"m004-probe","payload":{"n":1}}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("deliver to the unreachable webhook: status %d, want 202, body %s", status, raw)
	}

	// The drain wakes on the enqueue and the POST is refused in microseconds;
	// poll briefly so the assertion is about the queue, not about a race with
	// the drain loop.
	var depth, age float64
	var body string
	deadline := time.Now().Add(10 * time.Second)
	for {
		body = scrapeMetrics(t, baseURL)
		depth, _ = sampleValue(body, "crier_webhook_queue_depth")
		age, _ = sampleValue(body, "crier_webhook_queue_oldest_age_seconds")
		if depth >= 1 && age > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook backlog never became visible: depth=%v oldest_age=%v\nbody:\n%s", depth, age, body)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if depth < 1 {
		t.Errorf("crier_webhook_queue_depth = %v, want >= 1 while a refused delivery waits", depth)
	}
	if age <= 0 {
		t.Errorf("crier_webhook_queue_oldest_age_seconds = %v, want > 0 (the item was queued before this scrape)", age)
	}
	// The refused POST counted as an attempt (attempt=1 is the first drain).
	if attempts, ok := sampleValue(body, `crier_webhook_attempts_total{attempt="1"}`); !ok || attempts < 1 {
		t.Errorf(`crier_webhook_attempts_total{attempt="1"} = %v (present=%v), want >= 1 after one refused drain`, attempts, ok)
	}
}

// TestMetricsState_DetectionBootRendersAlertSeries proves the alert counter on a
// boot WITH detection configured, and that it advances when a canary trips —
// the rate the grow-only /alerts slice could not give.
func TestMetricsState_DetectionBootRendersAlertSeries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CR_ENABLE_METRICS", "true")
	t.Setenv("CR_ENABLE_PPROF", "")
	t.Setenv("CR_REQUIRE_AGENT_SIG", "false")
	t.Setenv("CR_DETECT_ENABLED", "true")
	t.Setenv("CR_DETECT_LOG", dir+"/delivery.jsonl")
	t.Setenv("CR_DETECT_KEY", dir+"/delivery.key")
	// Stable canaries: the token is known before the boot, so the trip can be
	// induced without reading /canaries.
	t.Setenv("CR_CANARY_TOKENS", "m004-canary-alpha")

	baseURL := bootObservabilityServer(t)
	body := scrapeMetrics(t, baseURL)

	const kind = `crier_detect_alerts_total{signal="canary_trip",severity="critical"}`
	before, ok := sampleValue(body, kind)
	if !ok {
		t.Fatalf("%s is absent on a detection-enabled boot\nbody:\n%s", kind, body)
	}
	if body := scrapeMetrics(t, baseURL); !strings.Contains(body, "# TYPE crier_detect_quarantined_current gauge") {
		t.Error("crier_detect_quarantined_current must be registered with detection on")
	}

	// A payload carrying the planted token is the exfiltration case the canary
	// exists for: it trips canary_trip (critical) on the delivery path.
	status, _, raw := bpCall(t, baseURL, http.MethodPost, "/agents",
		`{"id":"m004-canary-target"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("register canary target: status %d, body %s", status, raw)
	}
	status, _, raw = bpCall(t, baseURL, http.MethodPost, "/agents/m004-canary-target/inbox",
		`{"sender":"m004-canary-sender","payload":{"note":"m004-canary-alpha"}}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("deliver the canary payload: status %d, body %s", status, raw)
	}

	afterBody := scrapeMetrics(t, baseURL)
	after, ok := sampleValue(afterBody, kind)
	if !ok {
		t.Fatalf("%s disappeared after the trip\nbody:\n%s", kind, afterBody)
	}
	if after <= before {
		t.Errorf("%s = %v after a canary trip, want > %v (the alert must move the counter, not only the slice)", kind, after, before)
	}
	if q, _ := sampleValue(afterBody, "crier_detect_quarantined_current"); q != 0 {
		t.Errorf("crier_detect_quarantined_current = %v, want 0 — an alert is not a containment", q)
	}
}

// TestMetricsState_NilSubsystemsRenderZero pins the "nil object" contract: with
// no webhook driver, no federation links and no detector, every state gauge
// renders 0 — never NaN, never an absent series, never a panic.
func TestMetricsState_NilSubsystemsRenderZero(t *testing.T) {
	registerObservabilityGauges(observabilityDeps{})
	body := renderMetrics(t)

	for _, name := range []string{
		"crier_webhook_queue_depth",
		"crier_webhook_queue_oldest_age_seconds",
		"crier_federation_held_age_seconds",
		"crier_detect_quarantined_current",
	} {
		got, ok := sampleValue(body, name)
		if !ok {
			t.Errorf("%s is absent with nil deps\nbody:\n%s", name, body)
			continue
		}
		if math.IsNaN(got) {
			t.Errorf("%s = NaN with nil deps, want 0\nbody:\n%s", name, body)
			continue
		}
		if got != 0 {
			t.Errorf("%s = %v with nil deps, want 0", name, got)
		}
	}

	// The bounded counter children are seeded, so an idle process still exposes
	// them (a missing child would look like a metric that never fires).
	if _, ok := sampleValue(body, `crier_webhook_attempts_total{attempt="3"}`); !ok {
		t.Errorf("crier_webhook_attempts_total{attempt=\"3\"} is absent\nbody:\n%s", body)
	}
	if _, ok := sampleValue(body, `crier_detect_alerts_total{signal="fanout_spike",severity="critical"}`); !ok {
		t.Errorf("crier_detect_alerts_total{signal=\"fanout_spike\"} is absent\nbody:\n%s", body)
	}
}

// TestMetricsState_StoreDepthReadErrorsCountedOncePerScrape pins G8: the NaN
// path is now countable, once per scrape (not once per gauge), under the bounded
// backend label — while the gauges themselves still read NaN exactly as
// documented.
func TestMetricsState_StoreDepthReadErrorsCountedOncePerScrape(t *testing.T) {
	t.Run("failing store read", func(t *testing.T) {
		backing := "sqlite"
		registerQueueDepthGauges(func() (registry.QueueDepth, bool) {
			return registry.QueueDepth{}, false
		}, backing)

		before, _ := sampleValue(renderMetrics(t), `crier_store_queue_depth_read_errors_total{backend="sqlite"}`)
		body := renderMetrics(t)

		if !strings.Contains(body, "crier_inbox_queue_depth NaN") {
			t.Errorf("crier_inbox_queue_depth must stay NaN on a failed read\nbody:\n%s", body)
		}
		after, ok := sampleValue(body, `crier_store_queue_depth_read_errors_total{backend="sqlite"}`)
		if !ok {
			t.Fatalf("crier_store_queue_depth_read_errors_total{backend=\"sqlite\"} is absent\nbody:\n%s", body)
		}
		// renderMetrics evaluates all three inbox gauges; only the depth gauge
		// counts, so one failed scrape is one increment.
		if after-before != 1 {
			t.Errorf("read-error counter advanced by %v over one failed scrape, want exactly 1 (one per scrape, not one per gauge)", after-before)
		}
	})

	t.Run("store with no depth capability", func(t *testing.T) {
		// nil reader: the remote-proxy shape — permanently NaN, labelled proxy
		// so it cannot be mistaken for a flapping store.
		registerQueueDepthGauges(nil, "memory")
		before, _ := sampleValue(renderMetrics(t), `crier_store_queue_depth_read_errors_total{backend="proxy"}`)
		body := renderMetrics(t)
		after, ok := sampleValue(body, `crier_store_queue_depth_read_errors_total{backend="proxy"}`)
		if !ok {
			t.Fatalf("crier_store_queue_depth_read_errors_total{backend=\"proxy\"} is absent\nbody:\n%s", body)
		}
		if after-before != 1 {
			t.Errorf("proxy read-error counter advanced by %v over one scrape, want 1", after-before)
		}
		if !strings.Contains(body, "crier_inbox_queue_depth NaN") {
			t.Error("the documented NaN reading must be unchanged for a store with no depth route")
		}
	})

	// A healthy store must count nothing: the counter is the gap, not a
	// scrape counter.
	registerQueueDepthGauges(func() (registry.QueueDepth, bool) {
		return registry.QueueDepth{Pending: 3}, true
	}, "memory")
	before, _ := sampleValue(renderMetrics(t), `crier_store_queue_depth_read_errors_total{backend="memory"}`)
	body := renderMetrics(t)
	after, _ := sampleValue(body, `crier_store_queue_depth_read_errors_total{backend="memory"}`)
	if after != before {
		t.Errorf("a healthy depth read moved the read-error counter from %v to %v, want no movement", before, after)
	}
	if v, ok := sampleValue(body, "crier_inbox_queue_depth"); !ok || v != 3 {
		t.Errorf("crier_inbox_queue_depth = %v (present=%v), want the healthy reading 3", v, ok)
	}
}

// TestMetricsState_DepthBackendLabelIsBounded pins the label domain: the values
// are the resolved backends plus the two explained sentinels — nothing
// caller-supplied can reach the exposition.
func TestMetricsState_DepthBackendLabelIsBounded(t *testing.T) {
	healthy := func() (registry.QueueDepth, bool) { return registry.QueueDepth{}, true }
	cases := []struct {
		name    string
		read    queueDepthReader
		backend string
		want    string
	}{
		{"memory", healthy, "memory", "memory"},
		{"sqlite", healthy, "sqlite", "sqlite"},
		{"postgres", healthy, "postgres", "postgres"},
		{"no reader is the proxy shape", nil, "postgres", "proxy"},
		{"unresolved backend", healthy, "", "unknown"},
		{"never echoes a DSN", healthy, "postgres://u:p@h/db", "unknown"},
	}
	for _, c := range cases {
		if got := depthBackendLabel(c.read, c.backend); got != c.want {
			t.Errorf("%s: depthBackendLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestMetricsState_PrometheusTextStaysParseable is the cheap structural guard on
// the whole exposition: every sample line of this row is "name[{labels}] value",
// and the seven names all appear. (A full parser lives outside this repo; this
// catches the mistakes a hand-written exposition actually makes.)
func TestMetricsState_PrometheusTextStaysParseable(t *testing.T) {
	registerObservabilityGauges(observabilityDeps{})
	body := renderMetrics(t)

	for _, name := range stateSeriesNames {
		if !strings.Contains(body, "# HELP "+name+" ") && !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("%s has no HELP/TYPE block", name)
		}
		if !strings.Contains(body, "\n"+name) && !strings.HasPrefix(body, name) {
			t.Errorf("%s has no sample line", name)
		}
	}

	if strings.Contains(body, "\n\n") {
		t.Error("exposition contains a blank line between samples")
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Errorf("malformed exposition line %q, want \"name[{labels}] value\"", line)
			continue
		}
		if _, err := strconv.ParseFloat(fields[1], 64); err != nil {
			t.Errorf("exposition line %q carries a non-numeric value: %v", line, err)
		}
	}
}
