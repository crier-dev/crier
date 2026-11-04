package metrics

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// scrapeBody drives the registry through its real Handler over HTTP and
// returns the response body plus the Content-Type header, so tests pin the
// wire behaviour, not an internal helper.
func scrapeBody(t *testing.T, r *Registry) (string, string) {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b), resp.Header.Get("Content-Type")
}

func TestCounterAddInc(t *testing.T) {
	c := NewRegistry().NewCounter("x_total", "help")
	if got := c.value(); got != 0 {
		t.Fatalf("fresh counter = %v, want 0", got)
	}
	c.Inc()
	c.Add(3)
	if got := c.value(); got != 4 {
		t.Fatalf("counter = %v, want 4", got)
	}
	// Negative and zero deltas are ignored — counters never go down.
	c.Add(-1)
	c.Add(0)
	if got := c.value(); got != 4 {
		t.Fatalf("counter after negative/zero add = %v, want 4", got)
	}
}

func TestCounterVecChildrenAndDeterministicOrder(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewCounterVec("http_requests_total", "HTTP requests by status code.", "code")
	v.With("404").Inc()
	v.With("200").Add(2)
	v.With("200").Inc() // same child accumulates

	body, ctype := scrapeBody(t, reg)
	if ctype != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the v0.0.4 text format type", ctype)
	}
	for _, want := range []string{
		"# HELP http_requests_total HTTP requests by status code.",
		"# TYPE http_requests_total counter",
		`http_requests_total{code="200"} 3`,
		`http_requests_total{code="404"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
	// Deterministic order across scrapes: 200 sorts before 404.
	if strings.Index(body, `code="200"`) > strings.Index(body, `code="404"`) {
		t.Errorf("samples not in sorted label order:\n%s", body)
	}
}

func TestCounterWithZeroSamplesStillExposesMetadata(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("deliveries_total", "Accepted deliveries.")
	reg.NewCounterVec("webhook_deliveries_total", "Webhook push deliveries by outcome.", "outcome")

	// A plain counter always carries its single sample (0 until first
	// increment); a childless vec carries metadata only — both match the
	// reference client's exposition.
	body, _ := scrapeBody(t, reg)
	for _, want := range []string{
		"# TYPE deliveries_total counter",
		"deliveries_total 0",
		"# TYPE webhook_deliveries_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
	if strings.Contains(body, "webhook_deliveries_total{") {
		t.Errorf("childless vec emitted a sample anyway:\n%s", body)
	}
}

func TestLabelEscaping(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewCounterVec("w_total", "help", "outcome")
	v.With(`quo"te`).Inc()
	v.With(`back\slash`).Inc()
	v.With("line\nfeed").Inc()

	body, _ := scrapeBody(t, reg)
	for _, want := range []string{
		`w_total{outcome="quo\"te"} 1`,
		`w_total{outcome="back\\slash"} 1`,
		`w_total{outcome="line\nfeed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
	// The escaped form is the ONLY appearance: a raw newline would split the
	// sample stream into an unparsable line.
	if n := strings.Count(body, "line\\nfeed"); n != 1 {
		t.Errorf("escaped newline label appears %d times, want 1:\n%s", n, body)
	}
}

func TestGaugeFuncEvaluatedAtScrape(t *testing.T) {
	reg := NewRegistry()
	n := 0
	reg.RegisterGaugeFunc("depth", "help", func() float64 { n++; return float64(n) })

	if body, _ := scrapeBody(t, reg); !strings.Contains(body, "depth 1") {
		t.Fatalf("first scrape missing depth 1:\n%s", body)
	}
	if body, _ := scrapeBody(t, reg); !strings.Contains(body, "depth 2") {
		t.Fatalf("second scrape missing depth 2 (fn must be re-evaluated):\n%s", body)
	}
}

func TestReRegisterIdempotent(t *testing.T) {
	reg := NewRegistry()
	c1 := reg.NewCounter("dup_total", "help")
	c1.Inc()
	// A second boot in the same process gets the same series back.
	c2 := reg.NewCounter("dup_total", "help")
	c2.Inc()
	if got := c1.value(); got != 2 {
		t.Fatalf("re-registered counter = %v, want 2 (same series)", got)
	}
	if body, _ := scrapeBody(t, reg); strings.Count(body, "# TYPE dup_total counter") != 1 {
		t.Fatalf("# TYPE dup_total not exactly once:\n%s", body)
	}
}

func TestGaugeReRegisterReplaces(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterGaugeFunc("depth", "help", func() float64 { return 1 })
	reg.RegisterGaugeFunc("depth", "help", func() float64 { return 9 })
	body, _ := scrapeBody(t, reg)
	if !strings.Contains(body, "depth 9") || strings.Contains(body, "depth 1") {
		t.Fatalf("gauge re-register did not replace the previous fn:\n%s", body)
	}
	if strings.Count(body, "# TYPE depth gauge") != 1 {
		t.Fatalf("# TYPE depth not exactly once:\n%s", body)
	}
}

// TestGaugeFuncVec renders one sample under fixed labels, honours the
// registration order of the label names, evaluates fn per scrape and replaces
// the previous registration (the rebind a second in-process boot needs).
func TestGaugeFuncVec(t *testing.T) {
	reg := NewRegistry()
	n := 0
	reg.RegisterGaugeFuncVec("crier_build_info", "build identity",
		[]string{"version", "commit", "dirty"}, []string{"1.2.3", "abc12345", "false"},
		func() float64 { n++; return float64(n) })

	body, _ := scrapeBody(t, reg)
	if !strings.Contains(body, "# TYPE crier_build_info gauge") {
		t.Fatalf("missing gauge TYPE line:\n%s", body)
	}
	if !strings.Contains(body, `crier_build_info{version="1.2.3",commit="abc12345",dirty="false"} 1`) {
		t.Fatalf("labelled sample not rendered in registration order:\n%s", body)
	}
	if body, _ := scrapeBody(t, reg); !strings.Contains(body, `crier_build_info{version="1.2.3",commit="abc12345",dirty="false"} 2`) {
		t.Fatalf("fn must be re-evaluated at scrape:\n%s", body)
	}

	reg.RegisterGaugeFuncVec("crier_build_info", "build identity",
		[]string{"version", "commit", "dirty"}, []string{"9.9.9", "def67890", "true"},
		func() float64 { return 1 })
	body, _ = scrapeBody(t, reg)
	if strings.Count(body, "# TYPE crier_build_info gauge") != 1 {
		t.Fatalf("re-register duplicated the series:\n%s", body)
	}
	if !strings.Contains(body, `crier_build_info{version="9.9.9",commit="def67890",dirty="true"} 1`) {
		t.Fatalf("re-register did not replace the labels:\n%s", body)
	}
	if strings.Contains(body, `version="1.2.3"`) {
		t.Fatalf("stale label set survived re-registration:\n%s", body)
	}
}

// TestGaugeFuncVecLabelEscaping pins that a label value carrying a quote or a
// backslash is escaped, exactly like CounterVec's.
func TestGaugeFuncVecLabelEscaping(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterGaugeFuncVec("g", "help", []string{"commit"}, []string{`a"b\c`}, func() float64 { return 0 })
	body, _ := scrapeBody(t, reg)
	if !strings.Contains(body, `g{commit="a\"b\\c"} 0`) {
		t.Fatalf("label value not escaped:\n%s", body)
	}
}

// TestArityMismatchDiscarded covers the counter vec's deliberate discard on a
// wrong-arity With call: no panic, nothing in the exposition.
func TestArityMismatchDiscarded(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewCounterVec("z_total", "help", "a", "b")
	v.With("only-one").Inc()
	if body, _ := scrapeBody(t, reg); strings.Contains(body, "z_total{") {
		t.Fatalf("arity-mismatched child leaked into the exposition:\n%s", body)
	}
	v.With("one", "two").Inc() // correct arity works
	if body, _ := scrapeBody(t, reg); !strings.Contains(body, `z_total{a="one",b="two"} 1`) {
		t.Fatalf("correct-arity child missing:\n%s", body)
	}
}

func TestConcurrentScrapeAndIncrement(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounter("busy_total", "help")
	v := reg.NewCounterVec("busy_vec_total", "help", "k")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Inc()
				v.With("hot").Inc()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			_ = reg.render()
		}
	}()
	wg.Wait()
	if got := c.value(); got != 1600 {
		t.Fatalf("counter after concurrent increments = %v, want 1600", got)
	}
	if got := v.With("hot").value(); got != 1600 {
		t.Fatalf("vec child after concurrent increments = %v, want 1600", got)
	}
}

// TestHistogramRendersPrometheusShape pins the histogram exposition: bucket
// lines with an le label in ascending order, CUMULATIVE counts, a +Inf bucket,
// and the _sum/_count families a Prometheus server needs to build a p99
// (MON-CRIER-002).
func TestHistogramRendersPrometheusShape(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("crier_test_duration_seconds", "Test latency.", []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30})
	for _, v := range []float64{0.003, 0.02, 0.2, 40} {
		h.Observe(v)
	}

	body, _ := scrapeBody(t, reg)
	for _, want := range []string{
		"# TYPE crier_test_duration_seconds histogram",
		// 0.003 and 0.02 both land at or below 0.025; 0.2 is above 0.1 and
		// below 0.25; 40 exceeds the last bound and lives only in +Inf.
		`crier_test_duration_seconds_bucket{le="0.005"} 1`,
		`crier_test_duration_seconds_bucket{le="0.01"} 1`,
		`crier_test_duration_seconds_bucket{le="0.025"} 2`,
		`crier_test_duration_seconds_bucket{le="0.05"} 2`,
		`crier_test_duration_seconds_bucket{le="0.1"} 2`,
		`crier_test_duration_seconds_bucket{le="0.25"} 3`,
		`crier_test_duration_seconds_bucket{le="30"} 3`,
		`crier_test_duration_seconds_bucket{le="+Inf"} 4`,
		`crier_test_duration_seconds_count 4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "crier_test_duration_seconds_sum 40.223") {
		t.Errorf("sum sample missing or wrong (want 40.223); body:\n%s", body)
	}

	// Bucket lines must be strictly ascending by le and end at +Inf: a
	// Prometheus parser rejects a histogram whose buckets are out of order.
	prev := -1.0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "crier_test_duration_seconds_bucket{") {
			continue
		}
		le := line[strings.Index(line, `le="`)+4 : strings.Index(line, `"} `)]
		if le == "+Inf" {
			prev = math.Inf(1)
			continue
		}
		v, err := strconv.ParseFloat(le, 64)
		if err != nil {
			t.Fatalf("bucket le %q is not a float: %v", le, err)
		}
		if v <= prev {
			t.Errorf("bucket le=%v is not > the previous bound %v:\n%s", v, prev, body)
		}
		prev = v
	}
	if prev != math.Inf(1) {
		t.Errorf("histogram has no +Inf bucket as its last line:\n%s", body)
	}
}

// TestHistogramVecLabelsAndOrder pins the labelled form: the vec's labels in
// registration order, "le" last, one sample per child, sorted for a
// deterministic exposition.
func TestHistogramVecLabelsAndOrder(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewHistogramVec("crier_test_labelled_seconds", "help.",
		[]float64{0.1, 1}, "route", "method", "code")
	v.Observe(0.5, "/a", "GET", "200")
	v.Observe(0.05, "/b", "POST", "201")
	v.Observe(0.05, "/b", "POST", "201")

	body, _ := scrapeBody(t, reg)
	for _, want := range []string{
		"# TYPE crier_test_labelled_seconds histogram",
		`crier_test_labelled_seconds_bucket{route="/a",method="GET",code="200",le="0.1"} 0`,
		`crier_test_labelled_seconds_bucket{route="/a",method="GET",code="200",le="1"} 1`,
		`crier_test_labelled_seconds_bucket{route="/a",method="GET",code="200",le="+Inf"} 1`,
		`crier_test_labelled_seconds_sum{route="/a",method="GET",code="200"} 0.5`,
		`crier_test_labelled_seconds_count{route="/a",method="GET",code="200"} 1`,
		`crier_test_labelled_seconds_bucket{route="/b",method="POST",code="201",le="0.1"} 2`,
		`crier_test_labelled_seconds_count{route="/b",method="POST",code="201"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
	// le is the LAST label on a bucket line: a client library and every
	// Prometheus parser expects that position.
	if strings.Index(body, `route="/a"`) > strings.Index(body, `route="/b"`) {
		t.Errorf("children are not in sorted label order:\n%s", body)
	}
}

// TestHistogramBucketNormalisation: an unsorted bucket set with duplicates,
// zeros and negatives is normalised once at construction, so the exposition is
// always monotonic (and two calls with the same garbage still agree).
func TestHistogramBucketNormalisation(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("norm_seconds", "help", []float64{1, 0.1, 0.1, 0, -2, math.NaN(), 2.5})
	h.Observe(0.05)
	h.Observe(1.5)

	body, _ := scrapeBody(t, reg)
	if strings.Contains(body, `le="0"`) || strings.Contains(body, `le="-2"`) {
		t.Fatalf("non-positive bounds leaked into the exposition:\n%s", body)
	}
	if strings.Contains(body, `le="NaN"`) {
		t.Fatalf("a NaN bound rendered:\n%s", body)
	}
	for _, want := range []string{
		`norm_seconds_bucket{le="0.1"} 1`,
		`norm_seconds_bucket{le="1"} 1`,
		`norm_seconds_bucket{le="2.5"} 2`,
		`norm_seconds_bucket{le="+Inf"} 2`,
		"norm_seconds_count 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body:\n%s", want, body)
		}
	}
	if strings.Count(body, `le="0.1"`) != 1 {
		t.Errorf("duplicate bound rendered twice:\n%s", body)
	}
}

// TestHistogramReRegisterKeepsSeries: a second construction of the same name
// (a second boot in one process) must return the LIVE series, not duplicate
// the family or reset its buckets.
func TestHistogramReRegisterKeepsSeries(t *testing.T) {
	reg := NewRegistry()
	h1 := reg.NewHistogram("dup_seconds", "help", []float64{1})
	h1.Observe(0.5)
	h2 := reg.NewHistogram("dup_seconds", "help", []float64{5})
	h2.Observe(0.5)
	if got := h1.countValue(); got != 2 {
		t.Fatalf("re-registered histogram count = %d, want 2 (same series)", got)
	}
	v1 := reg.NewHistogramVec("dup_vec_seconds", "help", []float64{1}, "k")
	v1.Observe(0.5, "a")
	v2 := reg.NewHistogramVec("dup_vec_seconds", "help", []float64{9}, "k")
	v2.Observe(0.5, "a")

	body, _ := scrapeBody(t, reg)
	if n := strings.Count(body, "# TYPE dup_seconds histogram"); n != 1 {
		t.Errorf("# TYPE dup_seconds appears %d times, want 1:\n%s", n, body)
	}
	if n := strings.Count(body, "# TYPE dup_vec_seconds histogram"); n != 1 {
		t.Errorf("# TYPE dup_vec_seconds appears %d times, want 1:\n%s", n, body)
	}
	if !strings.Contains(body, `dup_vec_seconds_bucket{k="a",le="1"} 2`) {
		t.Errorf("re-registered vec did not keep accumulating:\n%s", body)
	}
}

// TestHistogramArityMismatchDiscarded: a wrong-arity With never panics and
// never leaks a half-labelled sample.
func TestHistogramArityMismatchDiscarded(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewHistogramVec("arity_seconds", "help", []float64{1}, "a", "b")
	v.With("only-one").Observe(0.5)
	body, _ := scrapeBody(t, reg)
	if strings.Contains(body, "arity_seconds_bucket{") {
		t.Fatalf("arity-mismatched child leaked into the exposition:\n%s", body)
	}
	v.Observe(0.5, "one", "two")
	body, _ = scrapeBody(t, reg)
	if !strings.Contains(body, `arity_seconds_bucket{a="one",b="two",le="+Inf"} 1`) {
		t.Fatalf("correct-arity child missing:\n%s", body)
	}
}

// TestHistogramConcurrentObserveAndScrape: writers and the renderer run
// concurrently without a race (run under -race) and every observation is
// counted exactly once.
func TestHistogramConcurrentObserveAndScrape(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewHistogramVec("busy_seconds", "help", DefaultBuckets, "k")
	v.Observe(0.5, "seed") // pre-create the child so the writers share it

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				v.Observe(0.01*float64(i+1), "hot")
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			_ = reg.render()
		}
	}()
	wg.Wait()

	if got := v.With("hot").countValue(); got != 1600 {
		t.Fatalf("child count after concurrent observes = %d, want 1600", got)
	}
	body, _ := scrapeBody(t, reg)
	if !strings.Contains(body, `busy_seconds_bucket{k="hot",le="+Inf"} 1600`) {
		t.Fatalf("+Inf bucket did not accumulate every observation:\n%s", body)
	}
}

// TestGaugeFuncLabelSamples pins the labelled fail-open gauge primitive
// (MON-CRIER-003): one sample per label value, deterministic order,
// evaluated at scrape time, and re-registration replacing the callback.
func TestGaugeFuncLabelSamples(t *testing.T) {
	reg := NewRegistry()
	values := map[string]float64{"b": 1, "a": 0}
	reg.RegisterGaugeFuncLabel("provider_open", "help text", "provider",
		func() map[string]float64 { return values })

	body, _ := scrapeBody(t, reg)
	if !strings.Contains(body, "# TYPE provider_open gauge") {
		t.Fatalf("labelled gauge missing its TYPE line:\n%s", body)
	}
	for _, want := range []string{
		`provider_open{provider="a"} 0` + "\n",
		`provider_open{provider="b"} 1` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition missing %q:\n%s", want, body)
		}
	}
	// Deterministic (sorted) sample order across scrapes.
	if strings.Index(body, `{provider="a"}`) > strings.Index(body, `{provider="b"}`) {
		t.Fatalf("samples are not sorted by label value:\n%s", body)
	}

	// Values are read at scrape time, not at registration.
	values["a"] = 1
	body, _ = scrapeBody(t, reg)
	if !strings.Contains(body, `provider_open{provider="a"} 1`) {
		t.Fatalf("gauge did not re-evaluate at scrape time:\n%s", body)
	}

	// Re-registering the name replaces the callback (no duplicate series).
	reg.RegisterGaugeFuncLabel("provider_open", "help text", "provider",
		func() map[string]float64 { return map[string]float64{"z": 7} })
	body, _ = scrapeBody(t, reg)
	if strings.Contains(body, `{provider="a"}`) || !strings.Contains(body, `provider_open{provider="z"} 7`) {
		t.Fatalf("re-registered gauge did not replace the previous callback:\n%s", body)
	}
	if got := strings.Count(body, "# TYPE provider_open gauge"); got != 1 {
		t.Fatalf("# TYPE provider_open appears %d times, want 1:\n%s", got, body)
	}
}

// ---------- MON-CRIER-005: dual-emit aliases ----------
//
// The namespacing migration is a dual-emit window: every legacy series keeps
// its name and samples while a namespaced twin is served from the SAME series
// (observability-spec §3.1). These tests pin the four properties the exposition
// depends on: identical samples, one evaluation per scrape, no duplicate
// family, and an alias that follows the live series.

// TestAliasRendersSameSamplesUnderBothNames: a counter and a labelled counter
// vec each expose their samples twice — under the legacy name and under the
// namespaced alias — with identical values and the same label set.
func TestAliasRendersSameSamplesUnderBothNames(t *testing.T) {
	reg := NewRegistry()
	deliveries := reg.NewCounter("deliveries_total", "Accepted deliveries.")
	deliveries.Add(7)
	guard := reg.NewCounterVec("guard_decisions_total", "Guard verdicts.", "decision")
	guard.With("block").Add(2)
	guard.With("allow").Add(5)

	reg.Alias("deliveries_total", "crier_delivery_accepted_total")
	reg.Alias("guard_decisions_total", "crier_guard_decisions_total")

	body, _ := scrapeBody(t, reg)
	for _, want := range []string{
		"deliveries_total 7",
		"crier_delivery_accepted_total 7",
		`guard_decisions_total{decision="allow"} 5`,
		`crier_guard_decisions_total{decision="allow"} 5`,
		`crier_guard_decisions_total{decision="block"} 2`,
		"# HELP crier_delivery_accepted_total Accepted deliveries.",
		"# TYPE crier_delivery_accepted_total counter",
		"# TYPE crier_guard_decisions_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
	// The alias is a NAME, not a copy: the family appears exactly once under
	// each name, and the underlying counter was never incremented twice.
	if got := strings.Count(body, "# TYPE crier_delivery_accepted_total counter"); got != 1 {
		t.Errorf("# TYPE crier_delivery_accepted_total appears %d times, want 1:\n%s", got, body)
	}
	if got := deliveries.value(); got != 7 {
		t.Errorf("aliasing changed the counter value: %v, want 7", got)
	}
}

// TestAliasGaugeEvaluatedOncePerScrape: a gauge callback runs ONCE per scrape
// even when the family is exported under two names. Several of this process's
// gauges have side effects (the inbox queue gauges read the serving store and
// count a failed read once per scrape), so an alias that re-evaluated the
// callback would double both the read and the error count.
func TestAliasGaugeEvaluatedOncePerScrape(t *testing.T) {
	reg := NewRegistry()
	calls := 0
	reg.RegisterGaugeFunc("inbox_queue_depth", "Queue depth.", func() float64 {
		calls++
		return 3
	})
	reg.Alias("inbox_queue_depth", "crier_inbox_queue_depth")

	body, _ := scrapeBody(t, reg)
	if calls != 1 {
		t.Fatalf("gauge callback ran %d times for one scrape, want 1", calls)
	}
	for _, want := range []string{"inbox_queue_depth 3", "crier_inbox_queue_depth 3"} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
}

// TestAliasFollowsLiveSeries: the alias is resolved at render time, so a gauge
// the composition layer re-registers on a second in-process boot keeps aliasing
// the LIVE callback instead of the object that existed when Alias was called.
func TestAliasFollowsLiveSeries(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterGaugeFunc("ws_subscribers", "Subscribers.", func() float64 { return 1 })
	reg.Alias("ws_subscribers", "crier_ws_subscribers")

	reg.RegisterGaugeFunc("ws_subscribers", "Subscribers.", func() float64 { return 42 })

	body, _ := scrapeBody(t, reg)
	for _, want := range []string{"ws_subscribers 42", "crier_ws_subscribers 42"} {
		if !strings.Contains(body, want) {
			t.Fatalf("alias did not follow the re-registered gauge (missing %q):\n%s", want, body)
		}
	}
	if strings.Contains(body, "ws_subscribers 1\n") {
		t.Fatalf("stale gauge value rendered after re-registration:\n%s", body)
	}
}

// TestAliasHistogramRenamesEveryDerivedSuffix: an aliased histogram renames all
// of its derived series (_bucket with le, _sum, _count) and keeps the label
// order, so Prometheus reads a real histogram under the new name.
func TestAliasHistogramRenamesEveryDerivedSuffix(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("legacy_duration_seconds", "Latency.", []float64{0.1, 1})
	h.Observe(0.05)
	h.Observe(2)
	reg.Alias("legacy_duration_seconds", "crier_legacy_duration_seconds")

	body, _ := scrapeBody(t, reg)
	for _, want := range []string{
		"# TYPE crier_legacy_duration_seconds histogram",
		`crier_legacy_duration_seconds_bucket{le="0.1"} 1`,
		`crier_legacy_duration_seconds_bucket{le="+Inf"} 2`,
		"crier_legacy_duration_seconds_count 2",
		"crier_legacy_duration_seconds_sum 2.05",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
}

// TestAliasIdempotentAndSingleName: aliasing the same pair twice (a second boot
// in one process) adds nothing, and a pair whose names are equal renders ONE
// family — the brief's rule that a series whose legacy name already matches its
// namespaced name is never emitted twice.
func TestAliasIdempotentAndSingleName(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("transfers_total", "help")
	reg.Alias("transfers_total", "crier_inbox_transfers_total")
	reg.Alias("transfers_total", "crier_inbox_transfers_total")

	body, _ := scrapeBody(t, reg)
	if got := strings.Count(body, "# TYPE crier_inbox_transfers_total counter"); got != 1 {
		t.Fatalf("# TYPE crier_inbox_transfers_total appears %d times, want 1 (idempotent alias):\n%s", got, body)
	}

	same := NewRegistry()
	same.NewCounter("crier_up", "help")
	same.Alias("crier_up", "crier_up") // equal names: no twin
	body, _ = scrapeBody(t, same)
	if got := strings.Count(body, "# TYPE crier_up gauge"); got != 0 {
		t.Errorf("unexpected family for a counter named crier_up:\n%s", body)
	}
	// Exactly one SAMPLE line (metadata lines also contain "crier_up ").
	if got := sampleLines(body, "crier_up "); got != 1 {
		t.Fatalf("equal-name alias duplicated the sample (%d lines):\n%s", got, body)
	}
}

// sampleLines counts the sample lines (not HELP/TYPE metadata) that begin with
// prefix — the exposition's own shape, not a substring guess.
func sampleLines(body, prefix string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestAliasYieldsToARealSeries: an alias is a name reservation, not ownership.
// When the namespaced name is later registered as a real series, that series
// wins and the alias is dropped — the exposition never serves one family twice.
func TestAliasYieldsToARealSeries(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("deliveries_total", "legacy")
	reg.Alias("deliveries_total", "crier_delivery_accepted_total")
	reg.NewCounter("crier_delivery_accepted_total", "real")

	body, _ := scrapeBody(t, reg)
	if got := strings.Count(body, "# TYPE crier_delivery_accepted_total counter"); got != 1 {
		t.Fatalf("# TYPE crier_delivery_accepted_total appears %d times, want 1:\n%s", got, body)
	}
	if !strings.Contains(body, "# HELP crier_delivery_accepted_total real") {
		t.Fatalf("the real series did not win over the alias:\n%s", body)
	}
}
