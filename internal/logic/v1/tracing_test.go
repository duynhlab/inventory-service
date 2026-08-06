package v1

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// lastSpan returns the most recently ended span with the given name. Tests run
// sequentially and each op ends its span (defer) before returning, so the last
// match is the one the call under test produced.
func lastSpan(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := testSpanRecorder.Ended()
	for i := len(ended) - 1; i >= 0; i-- {
		if ended[i].Name() == name {
			return ended[i]
		}
	}
	t.Fatalf("no span named %q recorded", name)
	return nil
}

// spanAttrs flattens a span's attributes into a key→string map.
func spanAttrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := make(map[string]string, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// exceptionMessages returns the exception.message value of every "exception"
// event on the span — the second channel (besides status description) through
// which span.RecordError exports text to the trace backend.
func exceptionMessages(s sdktrace.ReadOnlySpan) []string {
	var msgs []string
	for _, ev := range s.Events() {
		if ev.Name != "exception" {
			continue
		}
		for _, kv := range ev.Attributes {
			if string(kv.Key) == "exception.message" {
				msgs = append(msgs, kv.Value.Emit())
			}
		}
	}
	return msgs
}

// assertBoundedLogicSpan verifies BOTH channels of a logic span stay bounded:
//   - attributes are EXACTLY {layer=logic, operation, outcome} — no ids; and
//   - the status/exception channel (where a raw wrapped repository error would
//     otherwise bake in ids + SQLSTATE) carries only operation-derived text.
//
// For an error outcome the status must be Error with description "<op> failed"
// and a single "<op>_error" exception event; for any other outcome the status
// must be unset with no description and no exception events.
func assertBoundedLogicSpan(t *testing.T, s sdktrace.ReadOnlySpan, operation, outcome string) {
	t.Helper()
	attrs := spanAttrs(s)
	want := map[string]string{attrLayer: layerLogic, attrOperation: operation, attrOutcome: outcome}
	if len(attrs) != len(want) {
		t.Fatalf("span %q attributes = %v, want exactly %v (no ids)", s.Name(), attrs, want)
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("span %q attr %q = %q, want %q", s.Name(), k, attrs[k], v)
		}
	}

	desc := s.Status().Description
	msgs := exceptionMessages(s)
	if outcome == outcomeError {
		if s.Status().Code != codes.Error {
			t.Errorf("span %q status code = %v, want Error", s.Name(), s.Status().Code)
		}
		if desc != operation+" failed" {
			t.Errorf("span %q status description = %q, want bounded %q", s.Name(), desc, operation+" failed")
		}
		if len(msgs) != 1 || msgs[0] != operation+"_error" {
			t.Errorf("span %q exception messages = %v, want [%q]", s.Name(), msgs, operation+"_error")
		}
	} else {
		if s.Status().Code == codes.Error {
			t.Errorf("span %q status = Error on non-error outcome %q", s.Name(), outcome)
		}
		if desc != "" {
			t.Errorf("span %q carries status description %q on non-error outcome", s.Name(), desc)
		}
		if len(msgs) != 0 {
			t.Errorf("span %q recorded %d exception(s) on non-error outcome, want 0", s.Name(), len(msgs))
		}
	}
	// No raw SQLSTATE text may ride either channel regardless of outcome.
	for _, txt := range append(msgs, desc) {
		if strings.Contains(txt, "SQLSTATE") {
			t.Errorf("span %q leaks SQLSTATE via %q", s.Name(), txt)
		}
	}
}

// assertCanceledSpan verifies a caller hang-up (context.Canceled) is treated as
// "not our outcome": only {layer, operation} attributes, no outcome stamp, no
// error status, no exception event — matching every op's canceled handling.
func assertCanceledSpan(t *testing.T, s sdktrace.ReadOnlySpan, operation string) {
	t.Helper()
	attrs := spanAttrs(s)
	if attrs[attrLayer] != layerLogic || attrs[attrOperation] != operation {
		t.Errorf("span %q attrs = %v, want layer=logic operation=%q", s.Name(), attrs, operation)
	}
	if v, ok := attrs[attrOutcome]; ok {
		t.Errorf("canceled span %q stamped outcome=%q, want none (caller hang-up)", s.Name(), v)
	}
	if len(attrs) != 2 {
		t.Errorf("canceled span %q attrs = %v, want only {layer, operation}", s.Name(), attrs)
	}
	if s.Status().Code == codes.Error {
		t.Errorf("canceled span %q status = Error, want unset", s.Name())
	}
	if msgs := exceptionMessages(s); len(msgs) != 0 {
		t.Errorf("canceled span %q recorded exception(s) %v, want none", s.Name(), msgs)
	}
}

// TestRecordBoundedSpanError_NonRecordingSpanIsNoOp covers the IsRecording
// guard: with no active span the current span is non-recording, so the helper
// must be a safe no-op.
func TestRecordBoundedSpanError_NonRecordingSpanIsNoOp(t *testing.T) {
	recordBoundedSpanError(context.Background(), opReserve)
}

func TestReserve_EmitsBoundedLogicSpan(t *testing.T) {
	t.Run("success stamps outcome ok and no error status", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{result: domain.ReservationResult{Status: domain.ReservationReserved}})
		if _, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.reserve"), "reserve", "ok")
	})

	t.Run("infra error records a bounded error on the span", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{err: errors.New("db down")})
		if _, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"}); err == nil {
			t.Fatal("want error")
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.reserve"), "reserve", "error")
	})

	t.Run("business rejection does NOT record an error on the span", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{err: &domain.InsufficientStockError{}})
		if _, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"}); err == nil {
			t.Fatal("want error")
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.reserve"), "reserve", "insufficient")
	})
}

// TestSpanErrorChannel_NoRawErrorLeak is the regression test for the MEDIUM
// finding: a raw wrapped repository error carrying ids + SQLSTATE must never
// reach the span status description or an exception event. The span may only
// carry the bounded "<op> failed" / "<op>_error" text.
func TestSpanErrorChannel_NoRawErrorLeak(t *testing.T) {
	leaky := fmt.Errorf("commit balance sku-ABC/warehouse-7 res-XYZ: %w",
		errors.New("ERROR: deadlock detected (SQLSTATE 40P01)"))
	svc := NewReservationService(&fakeReservationRepo{err: leaky})
	if _, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"}); err == nil {
		t.Fatal("want error")
	}
	span := lastSpan(t, "inventory.reserve")

	if got := span.Status().Description; got != "reserve failed" {
		t.Errorf("status description = %q, want bounded 'reserve failed'", got)
	}

	// Collect every text the span exports and prove none of the raw tokens ride.
	texts := append([]string{span.Status().Description}, exceptionMessages(span)...)
	for _, ev := range span.Events() {
		for _, kv := range ev.Attributes {
			texts = append(texts, kv.Value.Emit())
		}
	}
	for _, bad := range []string{"sku-ABC", "warehouse-7", "res-XYZ", "SQLSTATE", "deadlock"} {
		for _, txt := range texts {
			if strings.Contains(txt, bad) {
				t.Errorf("span leaks %q via %q", bad, txt)
			}
		}
	}
}

func TestGetReservation_SpanRecordsInfraErrorOnly(t *testing.T) {
	t.Run("infra error records a bounded error on the span", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{err: errors.New("db down")})
		if _, err := svc.GetReservation(context.Background(), "res-1"); err == nil {
			t.Fatal("want error")
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.get_reservation"), "get_reservation", "error")
	})

	t.Run("not-found is a business outcome, not a span error", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{err: domain.ErrReservationNotFound})
		if _, err := svc.GetReservation(context.Background(), "ghost"); err == nil {
			t.Fatal("want error")
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.get_reservation"), "get_reservation", "not_found")
	})
}

func TestAvailability_EmitsBoundedLogicSpans(t *testing.T) {
	t.Run("check availability shortage", func(t *testing.T) {
		// tracked: the line is a real shortage, so the outcome stays `shortage`.
		svc := NewAvailabilityService(&fakeAvailabilityRepo{tracked: map[string]bool{"sku-a": true}})
		if _, err := svc.CheckAvailability(context.Background(), []CheckItem{{SKUID: "sku-a", Quantity: 1}}); err != nil {
			t.Fatalf("check: %v", err)
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.check_availability"), "check_availability", "shortage")
	})

	t.Run("check availability unknown sku is its own outcome", func(t *testing.T) {
		// Untracked, so the blocking reason is a data gap rather than a
		// stockout -- and an operator needs to see which one it was.
		svc := NewAvailabilityService(&fakeAvailabilityRepo{})
		if _, err := svc.CheckAvailability(context.Background(), []CheckItem{{SKUID: "sku-a", Quantity: 1}}); err != nil {
			t.Fatalf("check: %v", err)
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.check_availability"), "check_availability", "unknown_sku")
	})

	t.Run("check availability infra error records a bounded error", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{err: errors.New("db down")})
		if _, err := svc.CheckAvailability(context.Background(), []CheckItem{{SKUID: "sku-a", Quantity: 1}}); err == nil {
			t.Fatal("want error")
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.check_availability"), "check_availability", "error")
	})

	t.Run("batch get availability success", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{atp: map[string]int64{"sku-a": 5}})
		if _, err := svc.BatchGetAvailability(context.Background(), []string{"sku-a"}); err != nil {
			t.Fatalf("batch: %v", err)
		}
		assertBoundedLogicSpan(t, lastSpan(t, "inventory.batch_get_availability"), "batch_get_availability", "ok")
	})
}

// TestSpan_CanceledIsNotOurOutcome proves a caller hang-up leaves no outcome
// stamp and no span error on the read paths — consistent with the write paths.
func TestSpan_CanceledIsNotOurOutcome(t *testing.T) {
	t.Run("get reservation", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{err: fmt.Errorf("query: %w", context.Canceled)})
		if _, err := svc.GetReservation(context.Background(), "res-1"); err == nil {
			t.Fatal("want error")
		}
		assertCanceledSpan(t, lastSpan(t, "inventory.get_reservation"), "get_reservation")
	})

	t.Run("check availability", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{err: fmt.Errorf("query: %w", context.Canceled)})
		if _, err := svc.CheckAvailability(context.Background(), []CheckItem{{SKUID: "sku-a", Quantity: 1}}); err == nil {
			t.Fatal("want error")
		}
		assertCanceledSpan(t, lastSpan(t, "inventory.check_availability"), "check_availability")
	})
}

// TestReservationOutcome_DebugLog proves the G2 diagnostic trail: each write
// command logs its outcome at debug level with ONLY operation+outcome fields —
// no ids, no PII — and a business "no" is not escalated above debug.
func TestReservationOutcome_DebugLog(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	svc := NewReservationService(&fakeReservationRepo{result: domain.ReservationResult{Status: domain.ReservationReserved}}, WithLogger(logger))
	if _, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1", OrderID: "order-1"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	rej := NewReservationService(&fakeReservationRepo{err: &domain.InsufficientStockError{}}, WithLogger(logger))
	if _, err := rej.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"}); err == nil {
		t.Fatal("want error")
	}

	entries := logs.FilterMessage("reservation outcome").All()
	if len(entries) != 2 {
		t.Fatalf("got %d 'reservation outcome' logs, want 2", len(entries))
	}

	wantOutcomes := map[string]bool{"ok": false, "insufficient": false}
	for _, e := range entries {
		if e.Level != zapcore.DebugLevel {
			t.Errorf("log level = %v, want Debug (a business 'no' is not an operator error)", e.Level)
		}
		fields := e.ContextMap()
		if len(fields) != 2 {
			t.Errorf("log fields = %v, want exactly {operation, outcome} (no ids/PII)", fields)
		}
		op, _ := fields[attrOperation].(string)
		if op != "reserve" {
			t.Errorf("operation field = %v, want reserve", fields[attrOperation])
		}
		oc, _ := fields[attrOutcome].(string)
		if _, ok := wantOutcomes[oc]; !ok {
			t.Errorf("unexpected outcome field %q", oc)
			continue
		}
		wantOutcomes[oc] = true
	}
	for oc, seen := range wantOutcomes {
		if !seen {
			t.Errorf("missing debug log for outcome %q", oc)
		}
	}
}
