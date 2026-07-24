package v1

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// testMetricReader is the single manual reader every metric assertion in
// this package collects from. The global OTel delegation binds package-level
// instruments to the FIRST real provider installed — a second
// otel.SetMeterProvider would leave later readers blind — so the provider is
// installed exactly once, before any test runs.
var testMetricReader = sdkmetric.NewManualReader()

// testSpanRecorder captures every span the logic layer emits. Like the metric
// reader, the global tracer provider is installed exactly once (first-wins) so
// StartSpan delegates to this recorder for the whole package run.
var testSpanRecorder = tracetest.NewSpanRecorder()

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader)))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(testSpanRecorder)))
	os.Exit(m.Run())
}
