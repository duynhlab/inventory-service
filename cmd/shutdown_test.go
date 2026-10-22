package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestShutdownTelemetry_FreshBoundedContext pins the #1359 contract: the
// telemetry flush gets a live context with its own deadline, independent of
// however much of the HTTP drain budget was spent before it.
func TestShutdownTelemetry_FreshBoundedContext(t *testing.T) {
	const timeout = 5 * time.Second
	before := time.Now()
	called := false
	err := shutdownTelemetry(func(ctx context.Context) error {
		called = true
		if ctx.Err() != nil {
			t.Fatalf("shutdown context already done on entry: %v", ctx.Err())
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("shutdown context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > timeout {
			t.Fatalf("deadline %v from now, want within (0, %v]", remaining, timeout)
		}
		if deadline.Before(before) {
			t.Fatalf("deadline %v is before the call started %v", deadline, before)
		}
		return nil
	}, timeout)
	if err != nil {
		t.Fatalf("shutdownTelemetry returned %v, want nil", err)
	}
	if !called {
		t.Fatal("shutdown func was not called")
	}
}

// TestShutdownTelemetry_ReturnsExporterError pins that an exporter failure is
// surfaced to the caller rather than swallowed.
func TestShutdownTelemetry_ReturnsExporterError(t *testing.T) {
	want := errors.New("exporter unavailable")
	err := shutdownTelemetry(func(context.Context) error { return want }, time.Second)
	if !errors.Is(err, want) {
		t.Fatalf("shutdownTelemetry returned %v, want %v", err, want)
	}
}
