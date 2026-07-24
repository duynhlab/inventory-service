// Package middleware holds cross-cutting tracing helpers for inventory. The
// service is gRPC-only east-west (RFC-0021): the gRPC server is instrumented by
// the shared grpcx bootstrap, so this package exposes only the span helpers the
// logic layer uses to open `layer=logic` business spans under that transport
// span.
package middleware

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var (
	tracer          trace.Tracer
	tracerOnce      sync.Once
	detectedService string
)

// defaultServiceNameFallback names the tracer scope when SetServiceName was
// never called.
const defaultServiceNameFallback = "unknown-service"

// SetServiceName records the service name used by GetTracer for the tracer
// scope. The OTel SDK itself (providers, exporters, resource, sampler) is wired
// once in main() by obsx.SetupObservability (RFC-0014) — this package only
// consumes the globals it installs.
func SetServiceName(name string) {
	if name != "" {
		detectedService = name
	}
}

// GetTracer returns the tracer instance with auto-detected service name.
func GetTracer() trace.Tracer {
	tracerOnce.Do(func() {
		serviceName := detectedService
		if serviceName == "" {
			serviceName = defaultServiceNameFallback
		}
		tracer = otel.Tracer(serviceName)
	})
	return tracer
}

// StartSpan starts a new span with the given name.
//
// Usage:
//
//	ctx, span := middleware.StartSpan(ctx, "database.query")
//	defer span.End()
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	//nolint:spancheck // span is returned to caller who is responsible for calling span.End()
	return GetTracer().Start(ctx, name, opts...)
}

// Helper Functions

// AddSpanAttributes adds attributes to the current span if it's recording.
//
// Usage:
//
//	middleware.AddSpanAttributes(ctx,
//	    attribute.String("layer", "logic"),
//	    attribute.String("outcome", "ok"),
//	)
func AddSpanAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// AddSpanEvent adds an event to the current span if it's recording.
//
// Usage:
//
//	middleware.AddSpanEvent(ctx, "cache.hit",
//	    attribute.String("cache.key", key),
//	)
func AddSpanEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// RecordError records an error in the current span if it's recording.
//
// Usage:
//
//	if err != nil {
//	    middleware.RecordError(ctx, err)
//	    return err
//	}
func RecordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// SetSpanStatus sets the status of the current span if it's recording.
//
// Usage:
//
//	middleware.SetSpanStatus(ctx, codes.Ok, "request successful")
func SetSpanStatus(ctx context.Context, code codes.Code, description string) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetStatus(code, description)
	}
}
