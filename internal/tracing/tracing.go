package tracing

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const instrumentationName = "github.com/crier-dev/crier"

var enabled atomic.Bool

// Config controls opt-in trace export. The zero value disables tracing.
type Config struct {
	Enabled      bool
	Exporter     string
	OTLPEndpoint string
	SampleRatio  float64
	ServiceName  string
}

// Setup installs the global trace provider. Disabled mode leaves the SDK
// uninitialized and uses OpenTelemetry's process-wide no-op provider.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	enabled.Store(false)
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	if math.IsNaN(cfg.SampleRatio) || cfg.SampleRatio <= 0 || cfg.SampleRatio > 1 {
		return nil, fmt.Errorf("tracing: sample ratio must be in (0,1]")
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "crier"
	}
	if cfg.Exporter == "" {
		cfg.Exporter = "otlp"
	}
	var exporter sdktrace.SpanExporter
	var err error
	switch cfg.Exporter {
	case "stdout":
		exporter = newStdoutExporter()
	case "otlp":
		options := []otlptracehttp.Option{}
		if cfg.OTLPEndpoint != "" {
			options = append(options, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
		}
		exporter, err = otlptracehttp.New(ctx, options...)
	default:
		return nil, fmt.Errorf("tracing: unsupported exporter %q", cfg.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("tracing: create %s exporter: %w", cfg.Exporter, err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	enabled.Store(true)
	return func(ctx context.Context) error {
		err := provider.Shutdown(ctx)
		enabled.Store(false)
		return err
	}, nil
}
