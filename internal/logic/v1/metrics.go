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

	// reservation.total answers the saga-side on-call questions: are holds
	// failing on stock (insufficient), on retries (replayed/conflict), or on
	// the database (error)? Labels are bounded to enumerable domain values
	// (RFC-0017 D-9): operation and outcome only — no reservation or sku ids.
	reservationCounter, _ = meter.Int64Counter("inventory.reservation.total",
		metric.WithDescription("Reservation commands, split by operation and outcome"))
)

// Check outcomes (bounded).
const (
	outcomeFulfillable = "fulfillable"
	outcomeShortage    = "shortage"
	outcomeError       = "error"
)

// Reservation operations and outcomes (bounded).
const (
	opReserve = "reserve"
	opRelease = "release"
	opCommit  = "commit"

	outcomeOK                = "ok"
	outcomeReplayed          = "replayed"
	outcomeInsufficient      = "insufficient"
	outcomeConflict          = "conflict"    // idempotency: divergent payload under a used key
	outcomeConcurrency       = "concurrency" // lock/serialization abort: retryable, distinct signal
	outcomeInvalidTransition = "invalid_transition"
	outcomeNotFound          = "not_found"
)

// recordCheck counts one CheckAvailability call by its outcome.
func recordCheck(ctx context.Context, outcome string) {
	checkCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordReservation counts one reservation command by operation and outcome.
func recordReservation(ctx context.Context, operation, outcome string) {
	reservationCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.String("outcome", outcome)))
}
