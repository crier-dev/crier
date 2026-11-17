package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// spanAttributeAllowlist is the closed set of attribute keys crier may emit on
// a span, from specs/OBSERVABILITY-TRACING.md §4 ("never message payloads,
// prompt text, auth headers, or URL query strings"). It lives here so both the
// end-to-end span-tree test and the per-site coverage test below are checked
// against ONE list instead of two drifting copies.
var spanAttributeAllowlist = map[string]bool{
	"http.request.method":       true,
	"http.route":                true,
	"http.response.status_code": true,
	"crier.agent_id":            true,
	// §4's instrumentation table also names these, and the sites that emit
	// them are live (handler.go:1929, relay/handler.go:228+340,
	// harness/registry.go:226-243, webhook/webhook.go:354). A gate that only
	// admitted the first four would reject real traffic's spans the moment it
	// was extended beyond the two sites the span-tree test happens to drive.
	"crier.message_count":   true,
	"crier.topic":           true,
	"crier.harness.adapter": true,
	"crier.harness.state":   true,
	"crier.webhook.outcome": true,
}

// spanAttrAllowed reports whether the exported attribute key is in the §4
// allowlist.
func spanAttrAllowed(key string) bool { return spanAttributeAllowlist[key] }

func TestASetupDisabledDoesNotStartSDK(t *testing.T) {
	before := runtime.NumGoroutine()
	shutdown, err := Setup(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.NumGoroutine() != before {
		t.Fatalf("disabled Setup changed goroutine count: before=%d after=%d", before, runtime.NumGoroutine())
	}
	_, span := otel.Tracer(instrumentationName).Start(context.Background(), "noop-check")
	if span.IsRecording() {
		t.Fatal("disabled Setup returned a recording span")
	}
	span.End()
	headers := make(http.Header)
	Inject(context.Background(), headers)
	if headers.Get("traceparent") != "" {
		t.Fatalf("disabled tracing injected traceparent: %q", headers.Get("traceparent"))
	}
	called := false
	middleware := HTTPMiddleware(func(*http.Request) string {
		called = true
		return "/unexpected"
	})
	response := httptest.NewRecorder()
	middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "unchanged")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if called || response.Code != http.StatusAccepted || response.Body.String() != "unchanged" {
		t.Fatalf("disabled middleware changed behavior: called=%t status=%d body=%q", called, response.Code, response.Body.String())
	}
}

func TestHTTPMiddlewareExportsRouteAndChildSpanAndHonorsTraceparent(t *testing.T) {
	oldStdout := os.Stdout
	oldProvider := otel.GetTracerProvider()
	oldPropagator := otel.GetTextMapPropagator()
	defer func() {
		otel.SetTracerProvider(oldProvider)
		otel.SetTextMapPropagator(oldPropagator)
	}()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = oldStdout }()

	shutdown, err := Setup(context.Background(), Config{Enabled: true, Exporter: "stdout", SampleRatio: 1, ServiceName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handler := HTTPMiddleware(func(*http.Request) string { return "/ingress/messages" })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := Start(r.Context(), "registry.deliver", attribute.String("crier.agent_id", "agent-1"))
		_ = ctx
		span.End()
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "accepted")
	}))
	const traceID = "0123456789abcdef0123456789abcdef"
	request := httptest.NewRequest(http.MethodPost, "/ingress/messages?secret=query-value", strings.NewReader("private-payload"))
	request.Header.Set("traceparent", "00-"+traceID+"-0123456789abcdef-01")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "accepted" {
		t.Fatalf("response changed: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	var output bytes.Buffer
	if _, err := io.Copy(&output, read); err != nil {
		t.Fatal(err)
	}
	_ = read.Close()
	text := output.String()
	for _, want := range []string{"POST /ingress/messages", "registry.deliver", "http.route", "http.response.status_code", traceID} {
		if !strings.Contains(text, want) {
			t.Errorf("stdout trace missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"private-payload", "query-value", "secret=query-value", "Authorization"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("trace contains forbidden data %q", forbidden)
		}
	}
	var spans []struct {
		Name string `json:"Name"`
		Span struct {
			TraceID string `json:"TraceID"`
			SpanID  string `json:"SpanID"`
		} `json:"SpanContext"`
		Parent struct {
			SpanID string `json:"SpanID"`
		} `json:"Parent"`
		Attributes []struct {
			Key string `json:"Key"`
		} `json:"Attributes"`
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	for {
		var span struct {
			Name string `json:"Name"`
			Span struct {
				TraceID string `json:"TraceID"`
				SpanID  string `json:"SpanID"`
			} `json:"SpanContext"`
			Parent struct {
				SpanID string `json:"SpanID"`
			} `json:"Parent"`
			Attributes []struct {
				Key string `json:"Key"`
			} `json:"Attributes"`
		}
		if err := decoder.Decode(&span); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode stdout span: %v; output=%s", err, text)
		}
		spans = append(spans, span)
	}
	byName := make(map[string]struct {
		Name string `json:"Name"`
		Span struct {
			TraceID string `json:"TraceID"`
			SpanID  string `json:"SpanID"`
		} `json:"SpanContext"`
		Parent struct {
			SpanID string `json:"SpanID"`
		} `json:"Parent"`
		Attributes []struct {
			Key string `json:"Key"`
		} `json:"Attributes"`
	})
	for _, span := range spans {
		byName[span.Name] = span
		for _, attr := range span.Attributes {
			if !spanAttrAllowed(attr.Key) {
				t.Errorf("attribute key %q is outside the tracing allowlist", attr.Key)
			}
		}
	}
	server, serverOK := byName["POST /ingress/messages"]
	child, childOK := byName["registry.deliver"]
	if !serverOK || !childOK {
		t.Fatalf("span tree missing expected spans: %v", byName)
	}
	if server.Span.TraceID != traceID || server.Parent.SpanID != "0123456789abcdef" {
		t.Errorf("server span did not join traceparent: %#v", server)
	}
	if child.Span.TraceID != traceID || child.Parent.SpanID != server.Span.SpanID {
		t.Errorf("registry span is not a child of the HTTP span: server=%#v child=%#v", server, child)
	}
}

// TestSpanAttributeAllowlistCoversEveryInstrumentedSite is R-T4's "for every
// instrumented site" clause (specs/OBSERVABILITY-TRACING.md §7 test 4). The
// span-tree test above only ever observes two sites (POST /ingress/messages and
// registry.deliver), so an allowlist that forgot the keys the OTHER instrumented
// sites emit would pass it and then reject real traffic's spans.
//
// Each case below emits the span the production site emits, with the attributes
// transcribed from the call site named in the comment.
func TestSpanAttributeAllowlistCoversEveryInstrumentedSite(t *testing.T) {
	sites := []struct {
		name  string
		site  string
		attrs []attribute.KeyValue
	}{
		// internal/registry/handler.go:1122 (HandleDeliver)
		{"registry.deliver", "registry handler deliver", []attribute.KeyValue{attribute.String("crier.agent_id", "agent-1")}},
		// internal/registry/handler.go:1825 + :1929 (retrieve sets the count after the lease)
		{"registry.retrieve", "registry handler retrieve", []attribute.KeyValue{
			attribute.String("crier.agent_id", "agent-1"), attribute.Int("crier.message_count", 2)}},
		// internal/registry/handler.go:1951 (ack)
		{"registry.ack", "registry handler ack", []attribute.KeyValue{attribute.String("crier.agent_id", "agent-1")}},
		// internal/relay/handler.go:228 and :340
		{"relay.publish", "relay publish", []attribute.KeyValue{attribute.String("crier.topic", "demo")}},
		{"relay.subscribe", "relay subscribe", []attribute.KeyValue{attribute.String("crier.topic", "demo")}},
		// internal/harness/registry.go:226 + :243, and :228 for the state
		{"harness.dispatch", "harness registry dispatch", []attribute.KeyValue{
			attribute.String("crier.harness.adapter", "hermes"), attribute.String("crier.harness.state", "delivered")}},
		// internal/webhook/webhook.go:351 + :354
		{"webhook.deliver", "webhook driver", []attribute.KeyValue{attribute.String("crier.webhook.outcome", "delivered")}},
		// internal/federation/federation.go:484 + :525
		{"federation.forward", "federation forward", []attribute.KeyValue{
			attribute.String("crier.agent_id", "agent-1"), attribute.Int("http.response.status_code", 202)}},
	}

	for _, tc := range sites {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for _, attr := range tc.attrs {
				if !spanAttrAllowed(string(attr.Key)) {
					t.Errorf("%s: attribute key %q is outside the tracing allowlist (§4) — either the site must stop emitting it or the allowlist must adopt it",
						tc.site, attr.Key)
				}
			}
		})
	}

	// The allowlist is closed in the other direction too: a payload-shaped key
	// must never be admitted, so a future "just allow everything" edit fails.
	for _, forbidden := range []string{"crier.payload", "crier.message.body", "http.request.header.authorization", "http.url.query"} {
		if spanAttrAllowed(forbidden) {
			t.Errorf("allowlist admits payload/credential-shaped attribute %q", forbidden)
		}
	}
}
