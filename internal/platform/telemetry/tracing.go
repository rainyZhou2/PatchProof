package telemetry

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/platform/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

type Tracing struct {
	provider *sdktrace.TracerProvider
	enabled  bool
}

func InitTracing(ctx context.Context, cfg config.TelemetryConfig, service config.ServiceConfig) (*Tracing, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if cfg.OTLPEndpoint == "" {
		return &Tracing{enabled: false}, nil
	}
	options := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.OTLPInsecure {
		options = append(options, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(service.Name),
		semconv.ServiceVersion(service.Version),
		attribute.String("cloud.region", service.Region),
		attribute.String("patchproof.cluster", service.Cluster),
	))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(provider)
	return &Tracing{provider: provider, enabled: true}, nil
}

func (t *Tracing) Enabled() bool { return t != nil && t.enabled }

func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}
