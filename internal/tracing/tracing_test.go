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
			if attr.Key != "http.request.method" && attr.Key != "http.route" && attr.Key != "http.response.status_code" && attr.Key != "crier.agent_id" {
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
