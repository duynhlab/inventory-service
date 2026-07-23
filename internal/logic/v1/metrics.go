package v1

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Business metric for inventory, answering the on-call question a trace/log
// alone cannot: how often does the checkout gate answer "no" — is a shortage
// spike a stock problem or a database problem? → check.total{outcome}
//
// The instrument rides the global OTel MeterProvider that the observability
// setup installs (RFC-0014 OTLP pipeline → collector → VictoriaMetrics).
// Before that setup the global provider is a no-op, so package-init here is
// safe. The name is OTel-style; the collector renders it as
// inventory_check_total.
//
// Labels are bounded to enumerable domain values (RFC-0017 D-9): outcome
// only — no sku or warehouse ids.
var (
	meter = otel.Meter("inventory-service")

	checkCounter, _ = meter.Int64Counter("inventory.check.total",
		metric.WithDescription("Whole-basket availability checks, split by outcome"))
)

// Check outcomes (bounded).
const (
	outcomeFulfillable = "fulfillable"
	outcomeShortage    = "shortage"
	outcomeError       = "error"
)

// recordCheck counts one CheckAvailability call by its outcome.
func recordCheck(ctx context.Context, outcome string) {
	checkCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}
