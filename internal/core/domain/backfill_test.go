package domain_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

func TestBuildBackfillPlan_StraightCopyMapping(t *testing.T) {
	// At a drained cutover on_hand = stock_quantity, reserved = 0,
	// safety_stock = 0 — a straight copy. The reservation ledger is never read.
	plan := domain.BuildBackfillPlan([]domain.ProductRow{
		{ProductID: "1", StockQuantity: 100},
		{ProductID: "2", StockQuantity: 7},
		{ProductID: "3", StockQuantity: 0},
	})

	if len(plan.Mismatches) != 0 {
		t.Fatalf("want no mismatches, got %+v", plan.Mismatches)
	}
	want := []domain.BalanceTarget{
		{SKUID: "1", OnHand: 100, Reserved: 0, SafetyStock: 0},
		{SKUID: "2", OnHand: 7, Reserved: 0, SafetyStock: 0},
		{SKUID: "3", OnHand: 0, Reserved: 0, SafetyStock: 0},
	}
	if !reflect.DeepEqual(plan.Targets, want) {
		t.Fatalf("targets = %+v, want %+v", plan.Targets, want)
	}
}

func TestBuildBackfillPlan_PreservesInputOrder(t *testing.T) {
	plan := domain.BuildBackfillPlan([]domain.ProductRow{
		{ProductID: "10", StockQuantity: 1},
		{ProductID: "2", StockQuantity: 2},
		{ProductID: "1", StockQuantity: 3},
	})
	got := []string{plan.Targets[0].SKUID, plan.Targets[1].SKUID, plan.Targets[2].SKUID}
	want := []string{"10", "2", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (input order)", got, want)
	}
}

func TestBuildBackfillPlan_NegativeStockIsMismatch(t *testing.T) {
	// A negative stock_quantity is impossible under product's own CHECK; a torn
	// read that surfaces one is refused before any write, never mapped.
	plan := domain.BuildBackfillPlan([]domain.ProductRow{
		{ProductID: "1", StockQuantity: 50},
		{ProductID: "2", StockQuantity: -5},
	})

	if len(plan.Targets) != 1 || plan.Targets[0].SKUID != "1" {
		t.Fatalf("targets = %+v, want only sku 1", plan.Targets)
	}
	if len(plan.Mismatches) != 1 {
		t.Fatalf("want 1 mismatch, got %+v", plan.Mismatches)
	}
	m := plan.Mismatches[0]
	if m.SKUID != "2" || m.Reason != domain.MismatchNegativeStock {
		t.Errorf("mismatch = %+v, want sku 2 reason %s", m, domain.MismatchNegativeStock)
	}
}

func TestBuildBackfillPlan_Empty(t *testing.T) {
	plan := domain.BuildBackfillPlan(nil)
	if len(plan.Targets) != 0 || len(plan.Mismatches) != 0 {
		t.Fatalf("empty input must yield empty plan, got %+v", plan)
	}
}

// --- RunBackfill orchestrator (fakes, no DB) ---

type fakeProductReader struct {
	products []domain.ProductRow
	err      error
}

func (f *fakeProductReader) Products(context.Context) ([]domain.ProductRow, error) {
	return f.products, f.err
}

type fakeInventoryWriter struct {
	warehouseID  int64
	hasBalances  bool
	upsertCalls  int
	hasCalls     int
	lastRunID    string
	lastTargets  []domain.BalanceTarget
	changed      int
	warehouseErr error
	hasErr       error
	upsertErr    error
}

func (f *fakeInventoryWriter) DefaultWarehouseID(context.Context) (int64, error) {
	return f.warehouseID, f.warehouseErr
}

func (f *fakeInventoryWriter) HasBalances(context.Context) (bool, error) {
	f.hasCalls++
	return f.hasBalances, f.hasErr
}

func (f *fakeInventoryWriter) UpsertBalances(_ context.Context, _ int64, runID string, targets []domain.BalanceTarget) (int, error) {
	f.upsertCalls++
	f.lastRunID = runID
	f.lastTargets = targets
	return f.changed, f.upsertErr
}

func TestRunBackfill_DryRunWritesNothing(t *testing.T) {
	reader := &fakeProductReader{products: []domain.ProductRow{{ProductID: "1", StockQuantity: 100}}}
	writer := &fakeInventoryWriter{warehouseID: 1}

	report, err := domain.RunBackfill(context.Background(), reader, writer,
		domain.BackfillOptions{RunID: "run-1", Apply: false})
	if err != nil {
		t.Fatalf("dry-run error: %v", err)
	}
	if writer.upsertCalls != 0 || writer.hasCalls != 0 {
		t.Errorf("dry-run must not touch the writer, got upsert=%d has=%d", writer.upsertCalls, writer.hasCalls)
	}
	if report.Result != domain.ResultDryRun || report.Applied {
		t.Errorf("report = %+v, want dry-run result, not applied", report)
	}
	if report.ProductCount != 1 || report.TargetCount != 1 {
		t.Errorf("report counts = %+v", report)
	}
}

func TestRunBackfill_ApplyWritesTargets(t *testing.T) {
	reader := &fakeProductReader{products: []domain.ProductRow{{ProductID: "1", StockQuantity: 90}}}
	writer := &fakeInventoryWriter{warehouseID: 7, changed: 1}

	report, err := domain.RunBackfill(context.Background(), reader, writer,
		domain.BackfillOptions{RunID: "run-2", Apply: true})
	if err != nil {
		t.Fatalf("apply error: %v", err)
	}
	if writer.upsertCalls != 1 || writer.hasCalls != 1 {
		t.Fatalf("want 1 has-check + 1 upsert, got has=%d upsert=%d", writer.hasCalls, writer.upsertCalls)
	}
	if writer.lastRunID != "run-2" {
		t.Errorf("run id not threaded to writer: %q", writer.lastRunID)
	}
	want := []domain.BalanceTarget{{SKUID: "1", OnHand: 90, Reserved: 0, SafetyStock: 0}}
	if !reflect.DeepEqual(writer.lastTargets, want) {
		t.Errorf("upserted %+v, want %+v", writer.lastTargets, want)
	}
	if !report.Applied || report.Result != domain.ResultApplied || report.Changed != 1 || report.WarehouseID != 7 {
		t.Errorf("report = %+v", report)
	}
}

func TestRunBackfill_MismatchAbortsWithoutWrite(t *testing.T) {
	reader := &fakeProductReader{products: []domain.ProductRow{{ProductID: "1", StockQuantity: -1}}}
	writer := &fakeInventoryWriter{warehouseID: 1}

	report, err := domain.RunBackfill(context.Background(), reader, writer,
		domain.BackfillOptions{RunID: "run-3", Apply: true})
	if !errors.Is(err, domain.ErrBackfillMismatch) {
		t.Fatalf("want ErrBackfillMismatch, got %v", err)
	}
	if writer.upsertCalls != 0 || writer.hasCalls != 0 {
		t.Errorf("mismatch must abort before touching the writer")
	}
	if report.MismatchCount != 1 || report.Result != domain.ResultAbortedMismatch {
		t.Errorf("report = %+v", report)
	}
}

func TestRunBackfill_RefusesNonEmpty(t *testing.T) {
	reader := &fakeProductReader{products: []domain.ProductRow{{ProductID: "1", StockQuantity: 100}}}
	writer := &fakeInventoryWriter{warehouseID: 1, hasBalances: true}

	report, err := domain.RunBackfill(context.Background(), reader, writer,
		domain.BackfillOptions{RunID: "run-4", Apply: true})
	if !errors.Is(err, domain.ErrBalancesExist) {
		t.Fatalf("want ErrBalancesExist, got %v", err)
	}
	if writer.upsertCalls != 0 {
		t.Errorf("must never overwrite a populated table")
	}
	if report.Result != domain.ResultRefusedNonEmpty {
		t.Errorf("report result = %q, want %s", report.Result, domain.ResultRefusedNonEmpty)
	}
}

func TestRunBackfill_ApplyEmptyProductsFailsLoud(t *testing.T) {
	reader := &fakeProductReader{products: nil}
	writer := &fakeInventoryWriter{warehouseID: 1}

	report, err := domain.RunBackfill(context.Background(), reader, writer,
		domain.BackfillOptions{RunID: "run-5", Apply: true})
	if !errors.Is(err, domain.ErrNoProducts) {
		t.Fatalf("want ErrNoProducts, got %v", err)
	}
	if writer.hasCalls != 0 || writer.upsertCalls != 0 {
		t.Errorf("empty read must abort before touching the writer, got has=%d upsert=%d", writer.hasCalls, writer.upsertCalls)
	}
	if report.Result != domain.ResultNoProducts {
		t.Errorf("report result = %q, want %s", report.Result, domain.ResultNoProducts)
	}
}

func TestRunBackfill_ReaderErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	reader := &fakeProductReader{err: sentinel}
	writer := &fakeInventoryWriter{}

	_, err := domain.RunBackfill(context.Background(), reader, writer,
		domain.BackfillOptions{RunID: "run-6", Apply: true})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want reader error propagated, got %v", err)
	}
	if writer.upsertCalls != 0 {
		t.Errorf("reader failure must not write")
	}
}
