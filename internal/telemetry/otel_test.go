package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetupWithoutAnEndpointChangesNothing(t *testing.T) {
	meterBefore, tracerBefore := otel.GetMeterProvider(), otel.GetTracerProvider()

	shutdown, err := Setup(context.Background(), Settings{Environment: "development"})
	if err != nil {
		t.Fatalf("Setup() = %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() = %v, want nil", err)
	}

	if otel.GetMeterProvider() != meterBefore || otel.GetTracerProvider() != tracerBefore {
		t.Error("Setup with no endpoint replaced a global provider")
	}
	// Instrumentation must stay usable and silent.
	_, span := otel.Tracer("t").Start(context.Background(), "x")
	if span.SpanContext().IsValid() {
		t.Error("a span recorded with telemetry disabled")
	}
	span.End()
}
