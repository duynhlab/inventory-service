//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// TestAdminReads proves the protected list views over the real schema: the
// derived ATP arithmetic, the low-stock predicate, filter + paging math, and
// the ledger/reservation projections (RFC-0023 slice A).
func TestAdminReads(t *testing.T) {
	pool := newTestDB(t)
	commands := NewStockCommandRepository(pool)
	reads := NewAdminReadRepository(pool)
	ctx := context.Background()

	var wh int64
	if err := pool.QueryRow(ctx, `SELECT id FROM warehouses WHERE code = 'WH-DEFAULT'`).Scan(&wh); err != nil {
		t.Fatalf("default warehouse: %v", err)
	}

	// Three SKUs through the real command path: ample, low (atp <= safety),
	// and zero-stock. Reservations lift `reserved` via the reservation repo.
	seed := []domain.StockCommand{
		{CommandID: "ar-rcv-ample", SKUID: "ar-ample", WarehouseID: wh, Quantity: 100, Actor: "it"},
		{CommandID: "ar-rcv-low", SKUID: "ar-low", WarehouseID: wh, Quantity: 5, Actor: "it"},
		{CommandID: "ar-rcv-zero", SKUID: "ar-zero", WarehouseID: wh, Quantity: 3, Actor: "it"},
	}
	for _, cmd := range seed {
		if applied, err := commands.ReceiveStock(ctx, cmd); err != nil || !applied {
			t.Fatalf("seed receive %s = (%v, %v)", cmd.CommandID, applied, err)
		}
	}
	if applied, err := commands.SetSafetyStock(ctx, domain.StockCommand{
		CommandID: "ar-ss-low", SKUID: "ar-low", WarehouseID: wh, Quantity: 5, Actor: "it",
	}); err != nil || !applied {
		t.Fatalf("seed safety stock = (%v, %v)", applied, err)
	}
	if applied, err := commands.AdjustOnHand(ctx, domain.StockCommand{
		CommandID: "ar-adj-zero", SKUID: "ar-zero", WarehouseID: wh, Quantity: -3,
		Reason: "shrinkage", Actor: "it",
	}); err != nil || !applied {
		t.Fatalf("seed adjust = (%v, %v)", applied, err)
	}

	t.Run("sku balances derive atp and 404-shape untracked", func(t *testing.T) {
		rows, err := reads.SKUBalances(ctx, "ar-ample")
		if err != nil || len(rows) != 1 {
			t.Fatalf("sku balances = (%d rows, %v), want 1", len(rows), err)
		}
		b := rows[0]
		if b.OnHand != 100 || b.Reserved != 0 || b.ATP != 100 {
			t.Fatalf("ar-ample balance = %+v, want on_hand 100 atp 100", b)
		}
		if b.UpdatedAt == "" {
			t.Fatalf("updated_at must be RFC-3339, got empty")
		}

		empty, err := reads.SKUBalances(ctx, "ar-never-seen")
		if err != nil || len(empty) != 0 {
			t.Fatalf("untracked sku = (%d rows, %v), want empty slice", len(empty), err)
		}
	})

	t.Run("low-stock filter keeps atp <= safety_stock only", func(t *testing.T) {
		rows, total, err := reads.ListBalances(ctx, domain.BalanceFilter{LowStockOnly: true}, 100, 0)
		if err != nil {
			t.Fatalf("list low stock: %v", err)
		}
		got := map[string]bool{}
		for _, b := range rows {
			got[b.SKUID] = true
		}
		// ar-low: atp 5 <= safety 5. ar-zero: atp 0 <= safety 0.
		// ar-ample: atp 100 > safety 0 — must be absent.
		if !got["ar-low"] || !got["ar-zero"] || got["ar-ample"] {
			t.Fatalf("low-stock rows = %v (total %d), want ar-low+ar-zero without ar-ample", got, total)
		}
	})

	t.Run("balances page math", func(t *testing.T) {
		page1, total, err := reads.ListBalances(ctx, domain.BalanceFilter{}, 2, 0)
		if err != nil || total < 3 || len(page1) != 2 {
			t.Fatalf("page1 = (%d rows, total %d, %v), want 2 rows total>=3", len(page1), total, err)
		}
		page2, _, err := reads.ListBalances(ctx, domain.BalanceFilter{}, 2, 2)
		if err != nil || len(page2) == 0 {
			t.Fatalf("page2 = (%d rows, %v), want >0", len(page2), err)
		}
		if page1[0].SKUID == page2[0].SKUID {
			t.Fatalf("paging returned overlapping rows")
		}
	})

	t.Run("movement ledger projects commands newest-first with actor", func(t *testing.T) {
		rows, total, err := reads.ListMovements(ctx, domain.MovementFilter{SKUID: "ar-zero"}, 10, 0)
		if err != nil || total != 2 || len(rows) != 2 {
			t.Fatalf("ar-zero movements = (%d rows, total %d, %v), want 2", len(rows), total, err)
		}
		// Newest first: the ADJUST came after the RECEIVE.
		if rows[0].Type != "ADJUST" || rows[1].Type != "RECEIVE" {
			t.Fatalf("order = [%s, %s], want [ADJUST, RECEIVE]", rows[0].Type, rows[1].Type)
		}
		if rows[0].OnHandDelta != -3 || rows[0].Reason != "shrinkage" || rows[0].Actor != "it" {
			t.Fatalf("adjust row = %+v, want delta -3 reason shrinkage actor it", rows[0])
		}
	})

	t.Run("reservation headers list with status filter", func(t *testing.T) {
		resv := NewReservationRepository(pool)
		if _, err := resv.Reserve(ctx, domain.ReservationRequest{
			ID: "ar-res-1", OrderID: "ar-ord-1",
			Items: []domain.Line{{SKUID: "ar-ample", Quantity: 2}},
		}); err != nil {
			t.Fatalf("reserve: %v", err)
		}

		rows, total, err := reads.ListReservations(ctx, "reserved", 10, 0)
		if err != nil || total < 1 {
			t.Fatalf("list reservations = (total %d, %v), want >=1", total, err)
		}
		found := false
		for _, v := range rows {
			if v.ID == "ar-res-1" {
				found = true
				if v.OrderID != "ar-ord-1" || v.Status != "reserved" || v.CreatedAt == "" {
					t.Fatalf("reservation view = %+v", v)
				}
			}
		}
		if !found {
			t.Fatalf("ar-res-1 not in reserved page")
		}

		if _, total, err := reads.ListReservations(ctx, "released", 10, 0); err != nil || total != 0 {
			t.Fatalf("released filter = (total %d, %v), want 0", total, err)
		}

		// The reservation must also lift `reserved` in the balance view.
		bal, err := reads.SKUBalances(ctx, "ar-ample")
		if err != nil || len(bal) != 1 || bal[0].Reserved != 2 || bal[0].ATP != 98 {
			t.Fatalf("post-reserve balance = %+v (%v), want reserved 2 atp 98", bal, err)
		}
	})
}
