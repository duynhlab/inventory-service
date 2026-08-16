package v1

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/duynhlab/pkg/obsx"
)

// tracerScope names the instrumentation scope for logic spans. It is static on
// purpose: the scope says which code created the span, while the deployment
// identity (service.name) comes from the OTel resource that
// obsx.SetupObservability builds from OTEL_SERVICE_NAME and is stamped on every
// span regardless. The value matches that env var today, so the scope recorded
// here is unchanged from the package-level variable this replaced.
const tracerScope = "inventory"

// Span attribute keys and the fixed layer value. Kept as constants so the
// literals live in one place (goconst) and every logic span is stamped
// identically.
const (
	attrLayer     = "layer"
	attrOperation = "operation"
	attrOutcome   = "outcome"
	layerLogic    = "logic"
)

// Logic-span operation names not already covered by the metric operation
// constants (opReserve/opRelease/opCommit). These double as the span-name
// suffix and the bounded operation attribute.
const (
	opGetReservation    = "get_reservation"
	opCheckAvailability = "check_availability"
	opBatchAvailability = "batch_get_availability"
)

// startLogicSpan opens a `layer=logic` child span for one business operation,
// named inventory.<operation>. It carries only BOUNDED attributes (layer,
// operation); the caller stamps the outcome when the operation resolves. IDs
// (sku/order/reservation/warehouse) are deliberately never attached — they are
// unbounded and forbidden on inventory telemetry (see metrics.go), so they stay
// out of spans too. The returned context parents the repository's dbx/otelpgx
// spans under this one.
func startLogicSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return obsx.StartSpan(ctx, tracerScope, "inventory."+operation, trace.WithAttributes(
		attribute.String(attrLayer, layerLogic),
		attribute.String(attrOperation, operation),
	))
}

// setSpanOutcome stamps the bounded outcome on the current logic span.
func setSpanOutcome(ctx context.Context, outcome string) {
	obsx.AddSpanAttributes(ctx, attribute.String(attrOutcome, outcome))
}

// recordSpanError stamps the error outcome and records a BOUNDED infra failure
// on the current span. A canceled request (the caller hanging up) is neither
// stamped nor recorded — it is not our error.
func recordSpanError(ctx context.Context, operation string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	setSpanOutcome(ctx, outcomeError)
	recordBoundedSpanError(ctx, operation)
}

// recordBoundedSpanError records an infra failure on the current span using
// ONLY operation-derived text: status description "<operation> failed" and a
// synthetic "<operation>_error" exception event. The raw error is NEVER put on
// the span — wrapped repository errors bake in sku/order/reservation/warehouse
// ids and raw pgx/SQLSTATE text, which the span status description and
// exception.message event would export to the trace backend, defeating the
// fail-closed suppression the transport does on the wire. The raw error is
// still logged server-side by the transport's failClosed path, so
// debuggability is preserved. The caller stamps the outcome attribute.
func recordBoundedSpanError(ctx context.Context, operation string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.RecordError(errors.New(operation + "_error"))
	span.SetStatus(codes.Error, operation+" failed")
}
