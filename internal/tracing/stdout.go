package tracing

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// stdoutExporter is a small JSON SpanExporter used for local debugging. Keep it
// in this package so the tracing feature adds no exporter dependency beyond the
// OTLP HTTP exporter named by the tracing contract.
type stdoutExporter struct {
	mu      sync.Mutex
	encoder *json.Encoder
	stopped bool
}

func newStdoutExporter() trace.SpanExporter {
	return &stdoutExporter{encoder: json.NewEncoder(os.Stdout)}
}

func (e *stdoutExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	for _, span := range spans {
		if err := e.encoder.Encode(stdoutSpanFrom(span)); err != nil {
			return err
		}
	}
	return nil
}

func (e *stdoutExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	return nil
}

type stdoutSpan struct {
	Name        string               `json:"Name"`
	SpanContext stdoutSpanContext    `json:"SpanContext"`
	Parent      stdoutSpanContext    `json:"Parent"`
	SpanKind    string               `json:"SpanKind"`
	StartTime   time.Time            `json:"StartTime"`
	EndTime     time.Time            `json:"EndTime"`
	Attributes  []attribute.KeyValue `json:"Attributes"`
	Status      stdoutSpanStatus     `json:"Status"`
}

type stdoutSpanContext struct {
	TraceID    string `json:"TraceID"`
	SpanID     string `json:"SpanID"`
	TraceFlags string `json:"TraceFlags"`
	TraceState string `json:"TraceState"`
	Remote     bool   `json:"Remote"`
}

type stdoutSpanStatus struct {
	Code        string `json:"Code"`
	Description string `json:"Description"`
}

func stdoutSpanFrom(span trace.ReadOnlySpan) stdoutSpan {
	return stdoutSpan{
		Name:        span.Name(),
		SpanContext: stdoutContext(span.SpanContext()),
		Parent:      stdoutContext(span.Parent()),
		SpanKind:    span.SpanKind().String(),
		StartTime:   span.StartTime(),
		EndTime:     span.EndTime(),
		Attributes:  span.Attributes(),
		Status: stdoutSpanStatus{
			Code:        span.Status().Code.String(),
			Description: span.Status().Description,
		},
	}
}

func stdoutContext(sc oteltrace.SpanContext) stdoutSpanContext {
	return stdoutSpanContext{
		TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(),
		TraceFlags: sc.TraceFlags().String(), TraceState: sc.TraceState().String(), Remote: sc.IsRemote(),
	}
}
