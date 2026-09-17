package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
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

func TestArityMismatchDiscarded(t *testing.T) {
	reg := NewRegistry()
	v := reg.NewCounterVec("z_total", "help", "a", "b")
	// Wrong arity must not panic and must not register anything.
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
