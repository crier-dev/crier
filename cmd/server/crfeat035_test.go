// crfeat035_test.go — the CR-FEAT-035 acceptance run, against the REAL server
// wiring (run(nil), the real router, the real store, the real middleware), in
// the shape the row states:
//
//	(a) under queue pressure a higher-priority message is retrieved before a
//	    lower-priority one — and a delivery that names no priority is exactly
//	    the FIFO message it always was;
//	(b) a global limit sheds with 429 + Retry-After + a NAMED error, and does
//	    not exist at all when unconfigured;
//	(c) queue depth is visible where operators already look: GET /status and
//	    GET /metrics;
//	(d) the pre-existing per-agent publish cap keeps its behaviour and gains the
//	    Retry-After the review found missing.
//
// Everything here is a live HTTP call. Nothing is asserted through an internal
// seam that could disagree with what a client receives.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// bpEnv is the environment every boot in this file shares: signatures off (so a
// plain client can drive the agent-scoped routes, exactly as the quickstart
// documents for a single-user setup), the LLM guard off (no provider, no
// network), and the metrics surface on so (c) is measurable.
func bpEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_GUARD_ENABLED":     "false",
		"CR_ENABLE_METRICS":    "true",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// bpCall performs one HTTP request and returns the status, the headers and the
// raw body.
func bpCall(t *testing.T, baseURL, method, path, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("%s %s: build request: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

// bpRegister registers a keyless agent (legal because signatures are off).
func bpRegister(t *testing.T, baseURL, id string) {
	t.Helper()
	status, _, body := bpCall(t, baseURL, http.MethodPost, "/agents",
		fmt.Sprintf(`{"id":%q}`, id), nil)
	if status != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", id, status, body)
	}
}

// bpDeliver posts one delivery and returns the message id the server assigned.
// priority == "" omits the field entirely, which is the case that must behave
// exactly as it did before the field existed.
func bpDeliver(t *testing.T, baseURL, target, priority string) string {
	t.Helper()
	body := `{"sender":"producer","payload":{"n":1}`
	if priority != "" {
		body += `,"priority":` + priority
	}
	body += `}`
	status, _, raw := bpCall(t, baseURL, http.MethodPost, "/agents/"+target+"/inbox", body, nil)
	if status != http.StatusCreated {
		t.Fatalf("deliver to %s (priority %q): status %d, body %s", target, priority, status, raw)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &accepted); err != nil {
		t.Fatalf("deliver accept body %q is not JSON: %v", raw, err)
	}
	if accepted.ID == "" {
		t.Fatalf("deliver accept body %q carries no id", raw)
	}
	return accepted.ID
}

// bpRetrieveMessageIDs retrieves a batch and returns the message ids in the
// order the server handed them back, along with the raw body for the cases that
// assert on the wire shape.
func bpRetrieveMessageIDs(t *testing.T, baseURL, agent, query string) ([]string, string) {
	t.Helper()
	status, _, raw := bpCall(t, baseURL, http.MethodGet, "/agents/"+agent+"/inbox"+query, "", nil)
	if status != http.StatusOK {
		t.Fatalf("retrieve from %s: status %d, body %s", agent, status, raw)
	}
	var got struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("retrieve body %q is not JSON: %v", raw, err)
	}
	ids := make([]string, 0, len(got.Messages))
	for _, m := range got.Messages {
		ids = append(ids, m.ID)
	}
	return ids, raw
}

// TestCRFEAT035_PriorityBeatsArrivalOrderAndAbsentPriorityIsFIFO is acceptance
// (a), measured on one live server with one inbox.
func TestCRFEAT035_PriorityBeatsArrivalOrderAndAbsentPriorityIsFIFO(t *testing.T) {
	baseURL := bootStatusServer(t, bpEnv(map[string]string{
		// High enough that the global budget cannot interfere with this test.
		"CR_RATE_LIMIT_GLOBAL_PER_MINUTE": "1000",
	}))
	bpRegister(t, baseURL, "ordered")

	// Three low-value messages, then one urgent one behind them.
	low := []string{
		bpDeliver(t, baseURL, "ordered", "0"),
		bpDeliver(t, baseURL, "ordered", "0"),
		bpDeliver(t, baseURL, "ordered", "0"),
	}
	urgent := bpDeliver(t, baseURL, "ordered", "9")

	ids, raw := bpRetrieveMessageIDs(t, baseURL, "ordered", "?max=1&lease=1")
	if len(ids) != 1 || ids[0] != urgent {
		t.Fatalf("retrieve max=1 returned %v, want the urgent message %s behind the backlog (body %s)",
			ids, urgent, raw)
	}
	if !strings.Contains(raw, `"priority":9`) {
		t.Errorf("the retrieved body must carry the priority it was ordered by: %s", raw)
	}

	// The backlog still comes back in arrival order.
	rest, _ := bpRetrieveMessageIDs(t, baseURL, "ordered", "?max=10")
	if len(rest) != len(low) {
		t.Fatalf("retrieve max=10 returned %v, want the %d-message backlog", rest, len(low))
	}
	for i := range low {
		if rest[i] != low[i] {
			t.Fatalf("equal-priority backlog order = %v, want arrival order %v", rest, low)
		}
	}

	// Absent priority: no field, no reordering, no priority key on the wire.
	bpRegister(t, baseURL, "plain")
	plain := []string{
		bpDeliver(t, baseURL, "plain", ""),
		bpDeliver(t, baseURL, "plain", ""),
		bpDeliver(t, baseURL, "plain", ""),
	}
	ids, raw = bpRetrieveMessageIDs(t, baseURL, "plain", "?max=10")
	if len(ids) != len(plain) {
		t.Fatalf("retrieve of a priority-less inbox returned %v, want %v", ids, plain)
	}
	for i := range plain {
		if ids[i] != plain[i] {
			t.Fatalf("priority-less order = %v, want FIFO %v", ids, plain)
		}
	}
	if strings.Contains(raw, `"priority"`) {
		t.Errorf("a priority-less message must not grow a priority key: %s", raw)
	}

	// The documented range is enforced at the boundary, and nothing is stored.
	status, _, body := bpCall(t, baseURL, http.MethodPost, "/agents/plain/inbox",
		`{"payload":{"n":1},"priority":10}`, nil)
	if status != http.StatusBadRequest || !strings.Contains(body, "priority must be 0..9") {
		t.Fatalf("priority 10: status %d body %s, want 400 naming the range", status, body)
	}
}

// TestCRFEAT035_QueueDepthIsVisibleWhereOperatorsLook is acceptance (c): the
// live store-wide queue shows up in GET /status and GET /metrics.
func TestCRFEAT035_QueueDepthIsVisibleWhereOperatorsLook(t *testing.T) {
	baseURL := bootStatusServer(t, bpEnv(map[string]string{
		"CR_RATE_LIMIT_GLOBAL_PER_MINUTE": "500",
	}))
	bpRegister(t, baseURL, "queued")
	bpRegister(t, baseURL, "empty")

	const delivered = 4
	for i := 0; i < delivered; i++ {
		bpDeliver(t, baseURL, "queued", "1")
	}
	// One message leased: it is held, not delivered, so it stays queued.
	leased, _ := bpRetrieveMessageIDs(t, baseURL, "queued", "?max=1&lease=30")
	if len(leased) != 1 {
		t.Fatalf("expected exactly one leased message, got %v", leased)
	}

	// /status: the posture reports the budget, and the body carries the live
	// measurement next to it.
	status, _, body := getStatus(t, baseURL, "")
	if status != http.StatusOK {
		t.Fatalf("GET /status: status %d", status)
	}
	decoded := decodeStatus(t, body)
	if got := decoded["global_rate_limit_per_minute"]; got != float64(500) {
		t.Errorf("global_rate_limit_per_minute = %#v, want 500", got)
	}
	depth, ok := decoded["queue_depth"].(map[string]any)
	if !ok {
		t.Fatalf("queue_depth = %#v, want an object", decoded["queue_depth"])
	}
	if got := depth["pending"]; got != float64(delivered) {
		t.Errorf("queue_depth.pending = %#v, want %d", got, delivered)
	}
	if got := depth["leased"]; got != float64(1) {
		t.Errorf("queue_depth.leased = %#v, want 1", got)
	}
	if age, ok := depth["oldest_age_s"].(float64); !ok || age < 0 {
		t.Errorf("queue_depth.oldest_age_s = %#v, want a non-negative number", depth["oldest_age_s"])
	}

	// /metrics: the same measurement as gauges, with HELP/TYPE lines.
	status, _, metrics := bpCall(t, baseURL, http.MethodGet, "/metrics", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", status)
	}
	for _, want := range []string{
		"# TYPE inbox_queue_depth gauge",
		"# TYPE inbox_queue_leased gauge",
		"# TYPE inbox_queue_oldest_age_seconds gauge",
		"inbox_queue_depth " + strconv.Itoa(delivered),
		"inbox_queue_leased 1",
		"# TYPE inbox_shed_total counter",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("GET /metrics is missing %q:\n%s", want, metrics)
		}
	}
}

// TestCRFEAT035_GlobalBudgetShedsWithNamedErrorAndRetryAfter is acceptance (b),
// and it also measures (d): the pre-existing per-agent publish cap still refuses
// at its own limit — now with the Retry-After the review found missing.
func TestCRFEAT035_GlobalBudgetShedsWithNamedErrorAndRetryAfter(t *testing.T) {
	baseURL := bootStatusServer(t, bpEnv(map[string]string{
		"CR_RATE_LIMIT_GLOBAL_PER_MINUTE": "3",
		"CR_RATE_LIMIT_PER_MINUTE":        "2",
	}))
	bpRegister(t, baseURL, "producer")

	// The budget is GLOBAL: two different agents spend the same three slots.
	accepted := 0
	targets := []string{"producer", "producer", "producer", "producer"}
	for i, target := range targets {
		status, _, body := bpCall(t, baseURL, http.MethodPost, "/agents/"+target+"/inbox",
			`{"sender":"producer","payload":{"n":1}}`, nil)
		if status == http.StatusCreated {
			accepted++
			continue
		}
		if i < 3 {
			t.Fatalf("delivery %d answered %d (%s), want 201 while the budget lasts", i, status, body)
		}
		if status != http.StatusTooManyRequests {
			t.Fatalf("the delivery past the budget answered %d (%s), want 429", status, body)
		}
	}

	if accepted != 3 {
		t.Fatalf("accepted %d deliveries, want exactly the 3 the budget allows", accepted)
	}

	// The 4th, again, to inspect the shed response in full.
	status, headers, body := bpCall(t, baseURL, http.MethodPost, "/agents/producer/inbox",
		`{"payload":{"n":1}}`, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("shed delivery: status %d, want 429 (body %s)", status, body)
	}
	retryAfter := headers.Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("the 429 carries no Retry-After header — the gap the review named")
	}
	secs, err := strconv.Atoi(retryAfter)
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want an integer number of seconds >= 1", retryAfter)
	}
	var shed struct {
		Error          string `json:"error"`
		Scope          string `json:"scope"`
		LimitPerMinute int    `json:"limit_per_minute"`
		RetryAfterS    int    `json:"retry_after_s"`
	}
	if err := json.Unmarshal([]byte(body), &shed); err != nil {
		t.Fatalf("shed body %q is not JSON: %v", body, err)
	}
	if shed.Error != "RATE_LIMITED_GLOBAL" {
		t.Errorf("shed error = %q, want the NAMED error RATE_LIMITED_GLOBAL (body %s)", shed.Error, body)
	}
	if shed.Scope != "global" || shed.LimitPerMinute != 3 || shed.RetryAfterS != secs {
		t.Errorf("shed body = %+v, want scope global, limit 3 and retry_after_s %d", shed, secs)
	}

	// The budget exists to protect the queue: nothing it refused was stored.
	_, raw := bpRetrieveMessageIDs(t, baseURL, "producer", "?max=100")
	var queued struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &queued); err != nil {
		t.Fatalf("retrieve body %q is not JSON: %v", raw, err)
	}
	if len(queued.Messages) != accepted {
		t.Errorf("inbox holds %d message(s), want the %d accepted ones", len(queued.Messages), accepted)
	}

	// (d) The per-agent publish cap is unchanged in behaviour (100 events/minute
	// by default, 2 here) and its 429 now carries Retry-After too.
	publish := func() (int, string) {
		status, headers, body := bpCall(t, baseURL, http.MethodPost, "/relay/publish",
			`{"topic":"agent.status","event":{"ok":true}}`,
			map[string]string{"X-Agent-ID": "publisher"})
		return status, headers.Get("Retry-After") + "|" + strings.TrimSpace(body)
	}
	for i := 0; i < 2; i++ {
		if status, _ := publish(); status != http.StatusAccepted {
			t.Fatalf("publish %d: status %d, want 202 under the per-agent cap", i, status)
		}
	}
	status, combined := publish()
	if status != http.StatusTooManyRequests {
		t.Fatalf("the publish past the per-agent cap: status %d, want 429", status)
	}
	parts := strings.SplitN(combined, "|", 2)
	if parts[0] == "" {
		t.Errorf("the relay 429 carries no Retry-After (body %s)", parts[1])
	}
	if !strings.Contains(parts[1], "rate limit exceeded") {
		t.Errorf("the relay 429 body changed: %q, want the documented 'rate limit exceeded'", parts[1])
	}
}

// TestCRFEAT035_BudgetUnconfiguredShedsNothing is the "MUST NOT change existing
// behaviour when unused" half, measured on a live server: with no budget
// configured the delivery path refuses nothing, however many deliveries arrive,
// and /status reports the zero budget that says so.
func TestCRFEAT035_BudgetUnconfiguredShedsNothing(t *testing.T) {
	baseURL := bootStatusServer(t, bpEnv(nil))
	bpRegister(t, baseURL, "unbounded")

	const deliveries = 30
	for i := 0; i < deliveries; i++ {
		status, _, body := bpCall(t, baseURL, http.MethodPost, "/agents/unbounded/inbox",
			`{"payload":{"n":1}}`, nil)
		if status != http.StatusCreated {
			t.Fatalf("delivery %d: status %d (%s), want 201 with no budget configured", i, status, body)
		}
	}

	_, _, body := getStatus(t, baseURL, "")
	if got := decodeStatus(t, body)["global_rate_limit_per_minute"]; got != float64(0) {
		t.Errorf("global_rate_limit_per_minute = %#v, want 0 (no budget)", got)
	}
}

// TestCRFEAT035_QueueDepthGaugeIsNaNWhenTheStoreCannotReport pins the
// unavailability spelling: a store that cannot report a depth must not render
// as an empty queue. The reader is nil — the shape a non-DepthReporter store
// (the remote proxy) produces — and every gauge must then be NaN.
func TestCRFEAT035_QueueDepthGaugeIsNaNWhenTheStoreCannotReport(t *testing.T) {
	if got := statusQueueDepthFrom(nil); got != nil {
		t.Fatalf("a nil reader must render as null, got %+v", got)
	}

	registerQueueDepthGauges(nil)
	body := renderMetrics(t)
	for _, name := range []string{"inbox_queue_depth", "inbox_queue_leased", "inbox_queue_oldest_age_seconds"} {
		if !strings.Contains(body, name+" NaN") {
			t.Errorf("%s must render NaN when no store can report, body:\n%s", name, body)
		}
	}
}

// renderMetrics scrapes the process-wide exposition directly (the same registry
// GET /metrics serves), so a gauge registration can be asserted without booting
// a second server.
func renderMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
