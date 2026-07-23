package v1

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// testMetricReader is the single manual reader every metric assertion in
// this package collects from. The global OTel delegation binds package-level
// instruments to the FIRST real provider installed — a second
// otel.SetMeterProvider would leave later readers blind — so the provider is
// installed exactly once, before any test runs.
var testMetricReader = sdkmetric.NewManualReader()

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader)))
	os.Exit(m.Run())
}
