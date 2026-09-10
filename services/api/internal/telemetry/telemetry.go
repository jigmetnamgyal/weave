// Package telemetry bootstraps OpenTelemetry for the API service.
//
// The local baseline ships no network exporter. "none" installs a no-op
// tracer provider; "stdout" writes spans to the process stdout for local
// inspection. A collector endpoint is introduced with the first deployed
// environment, not here.
package telemetry

import (
	"context"
	"fmt"
	"io"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Exporter names accepted by Setup.
const (
	ExporterNone   = "none"
	ExporterStdout = "stdout"
)

// ShutdownFunc flushes and releases telemetry resources. It is always safe to
// call, including when tracing is disabled.
type ShutdownFunc func(ctx context.Context) error

// Setup installs the global tracer provider and propagator.
//
// W3C trace context propagation is registered regardless of exporter so that
// incoming trace headers survive this service even when spans are dropped.
func Setup(ctx context.Context, exporter, serviceName, env string, out io.Writer) (ShutdownFunc, error) {
	// Propagation is independent of sampling and export: register it first.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if exporter == ExporterNone {
		// The OTel API's default global provider is already a no-op, so there
		// is nothing to install and nothing to flush.
		return func(context.Context) error { return nil }, nil
	}

	if exporter != ExporterStdout {
		return nil, fmt.Errorf("unsupported trace exporter %q", exporter)
	}

	spanExporter, err := stdouttrace.New(
		stdouttrace.WithWriter(out),
		stdouttrace.WithPrettyPrint(),
	)
	if err != nil {
		return nil, fmt.Errorf("create stdout trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.DeploymentEnvironmentNameKey.String(env),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)

	return provider.Shutdown, nil
}
