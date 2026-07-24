//go:build integration

// Backfill apply-path integration test (RFC-0021 P2-2). It drives the
// domain.RunBackfill orchestrator with a FAKE product reader (product's schema
// is not stood up in-container — the read is modelled behind the interface) and
// the REAL BackfillRepository over a testcontainers Postgres, proving balances
// and the opening-balance movement ledger are written correctly, a re-run into a
// populated table is refused (no overwrite path), a mismatch/CHECK violation
// aborts without partial writes, the apply-clobber guard refuses a populated
// table without mutating it, and a missing default warehouse errors cleanly.
// Run with:
//
//	go test -tags=integration ./internal/core/repository/...
package repository

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// fakeProductReader stands in for product-service's DB.
type fakeProductReader struct {
	products []domain.ProductRow
}

func (f *fakeProductReader) Products(context.Context) ([]domain.ProductRow, error) {
	return f.products, nil
}

func TestBackfillApply_WritesBalancesAndLedger(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	reader := &fakeProductReader{products: []domain.ProductRow{
		{ProductID: "1", StockQuantity: 100},
		{ProductID: "2", StockQuantity: 50},
		{ProductID: "3", StockQuantity: 0},
	}}
	writer := NewBackfillRepository(pool)

	report, err := domain.RunBackfill(ctx, reader, writer, domain.BackfillOptions{RunID: "it-1", Apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Result != domain.ResultApplied || report.Changed != 3 {
		t.Fatalf("report = %+v, want applied changed=3", report)
	}

	// Balances: on_hand = stock_quantity, reserved = 0, safety_stock = 0.
	type bal struct{ onHand, reserved, safety int64 }
	want := map[string]bal{
		"1": {onHand: 100, reserved: 0, safety: 0},
		"2": {onHand: 50, reserved: 0, safety: 0},
		"3": {onHand: 0, reserved: 0, safety: 0},
	}
	for sku, w := range want {
		var got bal
		err := pool.QueryRow(ctx, `
			SELECT b.on_hand, b.reserved, b.safety_stock
			FROM inventory_balances b
			JOIN warehouses wh ON wh.id = b.warehouse_id AND wh.code = 'WH-DEFAULT'
			WHERE b.sku_id = $1`, sku).Scan(&got.onHand, &got.reserved, &got.safety)
		if err != nil {
			t.Fatalf("sku %s balance missing: %v", sku, err)
		}
		if got != w {
			t.Errorf("sku %s balance = %+v, want %+v", sku, got, w)
		}
	}

	// Ledger invariant: on_hand == SUM(on_hand_delta) per SKU, via a single
	// RECEIVE opening-balance movement with reference_type 'backfill'.
	for sku := range want {
		var deltaSum, moveCount, onHand int64
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(on_hand_delta), 0), COUNT(*)
			FROM inventory_movements
			WHERE sku_id = $1 AND type = 'RECEIVE' AND reference_type = 'backfill'`, sku).
			Scan(&deltaSum, &moveCount); err != nil {
			t.Fatalf("sku %s movement query: %v", sku, err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT on_hand FROM inventory_balances WHERE sku_id = $1`, sku).Scan(&onHand); err != nil {
			t.Fatalf("sku %s on_hand: %v", sku, err)
		}
		if moveCount != 1 {
			t.Errorf("sku %s: %d backfill movements, want exactly 1", sku, moveCount)
		}
		if deltaSum != onHand {
			t.Errorf("sku %s: SUM(on_hand_delta)=%d != on_hand=%d (ledger invariant)", sku, deltaSum, onHand)
		}
	}

	// Re-run into the now-populated table is refused (no overwrite path), so the
	// ledger cannot be corrupted by a second absolute copy: no new balances, no
	// duplicate movements.
	report2, err := domain.RunBackfill(ctx, reader, writer,
		domain.BackfillOptions{RunID: "it-1-again", Apply: true})
	if !errors.Is(err, domain.ErrBalancesExist) {
		t.Fatalf("re-run into populated table: want ErrBalancesExist, got %v", err)
	}
	if report2.Changed != 0 {
		t.Errorf("refused re-run changed = %d, want 0", report2.Changed)
	}
	var totalMoves int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM inventory_movements WHERE reference_type = 'backfill'`).Scan(&totalMoves); err != nil {
		t.Fatalf("count movements: %v", err)
	}
	if totalMoves != 3 {
		t.Errorf("after refused re-run: %d backfill movements, want 3", totalMoves)
	}
}

func TestBackfillApply_MismatchAbortsWithoutWrite(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	reader := &fakeProductReader{products: []domain.ProductRow{
		{ProductID: "1", StockQuantity: 100},
		{ProductID: "2", StockQuantity: -5}, // negative → mismatch
	}}
	writer := NewBackfillRepository(pool)

	_, err := domain.RunBackfill(ctx, reader, writer, domain.BackfillOptions{RunID: "it-2", Apply: true})
	if !errors.Is(err, domain.ErrBackfillMismatch) {
		t.Fatalf("want ErrBackfillMismatch, got %v", err)
	}
	assertEmpty(t, pool, "inventory_balances")
	assertEmpty(t, pool, "inventory_movements")
}

func TestBackfillDryRun_WritesNothing(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	reader := &fakeProductReader{products: []domain.ProductRow{{ProductID: "1", StockQuantity: 100}}}
	writer := NewBackfillRepository(pool)

	report, err := domain.RunBackfill(ctx, reader, writer, domain.BackfillOptions{RunID: "it-3", Apply: false})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if report.Result != domain.ResultDryRun {
		t.Errorf("result = %s, want dry-run", report.Result)
	}
	assertEmpty(t, pool, "inventory_balances")
	assertEmpty(t, pool, "inventory_movements")
}

// (a) A batch with one CHECK-violating row rolls back atomically: the repo
// returns an error and nothing is written, not even the valid rows before it.
func TestUpsertBalances_CheckViolationRollsBackAtomically(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	repo := NewBackfillRepository(pool)
	warehouseID, err := repo.DefaultWarehouseID(ctx)
	if err != nil {
		t.Fatalf("default warehouse: %v", err)
	}

	// The domain guard would reject on_hand < 0, so this drives the repo
	// directly to prove the transaction itself is all-or-nothing.
	targets := []domain.BalanceTarget{
		{SKUID: "ok", OnHand: 10, Reserved: 0, SafetyStock: 0},
		{SKUID: "bad", OnHand: -1, Reserved: 0, SafetyStock: 0}, // violates on_hand >= 0
	}
	if _, err := repo.UpsertBalances(ctx, warehouseID, "it-4", targets); err == nil {
		t.Fatal("want error from CHECK violation, got nil")
	}
	assertEmpty(t, pool, "inventory_balances")
	assertEmpty(t, pool, "inventory_movements")
}

// (b) A missing default warehouse errors cleanly.
func TestDefaultWarehouseID_MissingErrorsCleanly(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DELETE FROM warehouses WHERE code = 'WH-DEFAULT'`); err != nil {
		t.Fatalf("delete default warehouse: %v", err)
	}
	_, err := NewBackfillRepository(pool).DefaultWarehouseID(ctx)
	if err == nil {
		t.Fatal("want error for missing default warehouse, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should mention the warehouse is not found", err)
	}
}

// (c) The apply-clobber guard always refuses a populated table (there is no
// overwrite path), and leaks no partial write.
func TestBackfillApply_GuardRefusesNonEmpty(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	writer := NewBackfillRepository(pool)
	warehouseID, err := writer.DefaultWarehouseID(ctx)
	if err != nil {
		t.Fatalf("default warehouse: %v", err)
	}
	// Pre-populate a live balance the backfill must not clobber silently.
	if _, err := pool.Exec(ctx,
		`INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
		 VALUES ('live', $1, 5, 0, 3)`, warehouseID); err != nil {
		t.Fatalf("seed live balance: %v", err)
	}

	reader := &fakeProductReader{products: []domain.ProductRow{{ProductID: "1", StockQuantity: 100}}}

	_, err = domain.RunBackfill(ctx, reader, writer, domain.BackfillOptions{RunID: "it-5", Apply: true})
	if !errors.Is(err, domain.ErrBalancesExist) {
		t.Fatalf("want ErrBalancesExist, got %v", err)
	}
	// The new SKU was not written and the live balance is untouched.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM inventory_balances WHERE sku_id = '1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("guard leaked a write: sku 1 present")
	}
	var liveOnHand, liveReserved int64
	if err := pool.QueryRow(ctx,
		`SELECT on_hand, reserved FROM inventory_balances WHERE sku_id = 'live'`).Scan(&liveOnHand, &liveReserved); err != nil {
		t.Fatalf("read live balance: %v", err)
	}
	if liveOnHand != 5 || liveReserved != 0 {
		t.Errorf("live balance mutated: on_hand=%d reserved=%d, want 5/0", liveOnHand, liveReserved)
	}
}

func assertEmpty(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if n != 0 {
		t.Errorf("%s has %d rows, want 0", table, n)
	}
}
