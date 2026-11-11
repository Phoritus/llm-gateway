package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// Settings is what Setup needs from the service's configuration.
//
// OTEL_EXPORTER_OTLP_HEADERS, which holds the Grafana Cloud token, is absent
// on purpose. The exporters read it from the environment themselves, so it
// never passes through a value this service could log.
type Settings struct {
	// Endpoint is OTEL_EXPORTER_OTLP_ENDPOINT. Empty disables telemetry.
	Endpoint string
	// Environment is APP_ENV, recorded as deployment.environment.
	Environment string
}

// Setup installs the global meter and tracer providers and the W3C
// propagator, and returns the function that flushes and stops them.
//
// With no endpoint nothing is installed: the global providers stay the
// library's no-ops, so instrumentation written against otel.Meter costs
// nothing and needs no setup in tests.
//
// Exporter protocol, sampler and headers come from the standard OTEL_*
// variables, which is what lets a deployment move from the local stack to
// Grafana Cloud without a code change.
func Setup(ctx context.Context, s Settings) (shutdown func(context.Context) error, err error) {
	if s.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "llm-gateway"
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		attribute.String("deployment.environment", s.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)

	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		return errors.Join(mp.Shutdown(ctx), tp.Shutdown(ctx))
	}, nil
}
