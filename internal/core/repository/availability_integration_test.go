//go:build integration

// Availability-read integration tests (RFC-0021 P1-4). They prove the ATP
// derivation against a real Postgres: the GREATEST(0, …) floor, the
// safety-stock subtraction, the active-warehouse filter, and that absent
// SKUs stay absent (never fabricated as zero rows).
package repository

import (
	"context"
	"reflect"
	"testing"
)

func TestAvailabilityRepository(t *testing.T) {
	pool := newTestDB(t)
	repo := NewAvailabilityRepository(pool)
	ctx := context.Background()

	var defaultWH, secondWH, inactiveWH, emptyWH int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM warehouses WHERE code = 'WH-DEFAULT'`).Scan(&defaultWH); err != nil {
		t.Fatalf("default warehouse missing: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO warehouses (code, name, status) VALUES ('WH-2', 'Second', 'active') RETURNING id`).
		Scan(&secondWH); err != nil {
		t.Fatalf("insert second warehouse: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO warehouses (code, name, status) VALUES ('WH-OFF', 'Closed', 'inactive') RETURNING id`).
		Scan(&inactiveWH); err != nil {
		t.Fatalf("insert inactive warehouse: %v", err)
	}
	// Active but holding no balance rows: it must still appear in the active
	// id list — it is the shortage baseline candidate the logic layer needs.
	if err := pool.QueryRow(ctx,
		`INSERT INTO warehouses (code, name, status) VALUES ('WH-EMPTY', 'Empty', 'active') RETURNING id`).
		Scan(&emptyWH); err != nil {
		t.Fatalf("insert empty warehouse: %v", err)
	}

	// sku-plain: 10-2-0=8 in default + 3-0-0=3 in second, plus stock in the
	// inactive warehouse that must never count. sku-safety: safety stock
	// subtracts (10-2-5=3). sku-floored: reserved+safety exceed on_hand, so
	// the per-warehouse floor pins ATP at 0. sku-off-only exists only in the
	// inactive warehouse — promisable nowhere.
	seed := []struct {
		sku                          string
		wh, onHand, reserved, safety int64
	}{
		{"sku-plain", defaultWH, 10, 2, 0},
		{"sku-plain", secondWH, 3, 0, 0},
		{"sku-plain", inactiveWH, 100, 0, 0},
		{"sku-safety", defaultWH, 10, 2, 5},
		{"sku-floored", defaultWH, 5, 5, 3},
		{"sku-off-only", inactiveWH, 50, 0, 0},
	}
	for _, b := range seed {
		if _, err := pool.Exec(ctx,
			`INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
			 VALUES ($1, $2, $3, $4, $5)`,
			b.sku, b.wh, b.onHand, b.reserved, b.safety); err != nil {
			t.Fatalf("seed balance %s/wh=%d: %v", b.sku, b.wh, err)
		}
	}

	t.Run("BatchATP sums active warehouses only", func(t *testing.T) {
		atp, err := repo.BatchATP(ctx,
			[]string{"sku-plain", "sku-safety", "sku-floored", "sku-off-only", "sku-missing"})
		if err != nil {
			t.Fatalf("BatchATP: %v", err)
		}
		want := map[string]int64{
			"sku-plain":   11, // 8 (default) + 3 (second); inactive 100 excluded
			"sku-safety":  3,  // 10 - 2 - 5
			"sku-floored": 0,  // GREATEST(0, 5-5-3)
		}
		for sku, w := range want {
			if got, ok := atp[sku]; !ok || got != w {
				t.Errorf("atp[%s] = %d (present=%v), want %d", sku, got, ok, w)
			}
		}
		for _, sku := range []string{"sku-off-only", "sku-missing"} {
			if _, ok := atp[sku]; ok {
				t.Errorf("atp[%s] present, want absent", sku)
			}
		}
	})

	t.Run("ActiveWarehouseBalances splits per warehouse", func(t *testing.T) {
		balances, activeIDs, err := repo.ActiveWarehouseBalances(ctx,
			[]string{"sku-plain", "sku-safety", "sku-off-only"})
		if err != nil {
			t.Fatalf("ActiveWarehouseBalances: %v", err)
		}
		// Every active warehouse in ascending id order — including the empty
		// one (shortage baseline), excluding the inactive one.
		wantIDs := []int64{defaultWH, secondWH, emptyWH}
		if !reflect.DeepEqual(activeIDs, wantIDs) {
			t.Errorf("activeIDs = %v, want %v", activeIDs, wantIDs)
		}
		if _, ok := balances[inactiveWH]; ok {
			t.Errorf("inactive warehouse %d present, want excluded", inactiveWH)
		}
		if _, ok := balances[emptyWH]; ok {
			t.Errorf("empty warehouse %d has balances, want none", emptyWH)
		}
		if got := balances[defaultWH]["sku-plain"]; got != 8 {
			t.Errorf("default/sku-plain = %d, want 8", got)
		}
		if got := balances[defaultWH]["sku-safety"]; got != 3 {
			t.Errorf("default/sku-safety = %d, want 3", got)
		}
		if got := balances[secondWH]["sku-plain"]; got != 3 {
			t.Errorf("second/sku-plain = %d, want 3", got)
		}
		if _, ok := balances[secondWH]["sku-safety"]; ok {
			t.Errorf("second/sku-safety present, want absent (no balance row)")
		}
	})

	t.Run("TrackedSKUs spans active and inactive warehouses", func(t *testing.T) {
		tracked, err := repo.TrackedSKUs(ctx,
			[]string{"sku-plain", "sku-off-only", "sku-missing"})
		if err != nil {
			t.Fatalf("TrackedSKUs: %v", err)
		}
		// sku-off-only lives only in the inactive warehouse: tracked (it maps
		// to OUT_OF_STOCK upstream), while sku-missing stays untracked.
		want := map[string]bool{"sku-plain": true, "sku-off-only": true}
		if !reflect.DeepEqual(tracked, want) {
			t.Errorf("tracked = %v, want %v", tracked, want)
		}
	})
}
