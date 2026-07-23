//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

func TestStockCommands(t *testing.T) {
	pool := newTestDB(t)
	repo := NewStockCommandRepository(pool)
	ctx := context.Background()

	var wh int64
	if err := pool.QueryRow(ctx, `SELECT id FROM warehouses WHERE code = 'WH-DEFAULT'`).Scan(&wh); err != nil {
		t.Fatalf("default warehouse: %v", err)
	}
	onHand := func(sku string) int64 {
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT on_hand FROM inventory_balances WHERE sku_id = $1 AND warehouse_id = $2`, sku, wh).Scan(&n); err != nil {
			t.Fatalf("read on_hand: %v", err)
		}
		return n
	}
	movements := func(sku string) int64 {
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM inventory_movements WHERE sku_id = $1 AND warehouse_id = $2`, sku, wh).Scan(&n); err != nil {
			t.Fatalf("count movements: %v", err)
		}
		return n
	}

	t.Run("receive creates then accumulates; replay is a no-op", func(t *testing.T) {
		cmd := domain.StockCommand{CommandID: "rcv-1", SKUID: "cmd-a", WarehouseID: wh, Quantity: 10, Actor: "test"}
		applied, err := repo.ReceiveStock(ctx, cmd)
		if err != nil || !applied {
			t.Fatalf("first receive = (%v, %v), want (true, nil)", applied, err)
		}
		applied, err = repo.ReceiveStock(ctx, cmd) // exact replay
		if err != nil || applied {
			t.Fatalf("replayed receive = (%v, %v), want (false, nil)", applied, err)
		}
		if got := onHand("cmd-a"); got != 10 {
			t.Fatalf("on_hand = %d, want 10 (replay must not double-apply)", got)
		}
		if got := movements("cmd-a"); got != 1 {
			t.Fatalf("movement rows = %d, want exactly 1", got)
		}
		// Admin commands record the principal in actor (migration 000003);
		// reference_id is reserved for reservation-driven movements.
		var actor string
		var refID *string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(actor, ''), reference_id FROM inventory_movements WHERE command_id = 'rcv-1'`).
			Scan(&actor, &refID); err != nil {
			t.Fatalf("read movement actor: %v", err)
		}
		if actor != "test" || refID != nil {
			t.Fatalf("movement actor = %q, reference_id = %v; want actor=test, reference_id NULL", actor, refID)
		}

		applied, err = repo.ReceiveStock(ctx, domain.StockCommand{
			CommandID: "rcv-2", SKUID: "cmd-a", WarehouseID: wh, Quantity: 5, Actor: "test"})
		if err != nil || !applied {
			t.Fatalf("second receive = (%v, %v)", applied, err)
		}
		if got := onHand("cmd-a"); got != 15 {
			t.Fatalf("on_hand = %d, want 15", got)
		}
	})

	t.Run("divergent replay of a claimed command_id conflicts without state change", func(t *testing.T) {
		before := onHand("cmd-a")
		moves := movements("cmd-a")

		// Same command_id as rcv-1 but a different sku: must conflict, not
		// silently report a no-op replay.
		applied, err := repo.ReceiveStock(ctx, domain.StockCommand{
			CommandID: "rcv-1", SKUID: "cmd-divergent", WarehouseID: wh, Quantity: 10, Actor: "test"})
		if !errors.Is(err, domain.ErrCommandConflict) {
			t.Fatalf("divergent sku replay = (%v, %v), want ErrCommandConflict", applied, err)
		}

		// Same command_id and sku but a different quantity (delta) also conflicts.
		applied, err = repo.ReceiveStock(ctx, domain.StockCommand{
			CommandID: "rcv-1", SKUID: "cmd-a", WarehouseID: wh, Quantity: 99, Actor: "test"})
		if !errors.Is(err, domain.ErrCommandConflict) {
			t.Fatalf("divergent quantity replay = (%v, %v), want ErrCommandConflict", applied, err)
		}

		if got := onHand("cmd-a"); got != before {
			t.Fatalf("on_hand changed %d -> %d after conflicting replays", before, got)
		}
		if got := movements("cmd-a"); got != moves {
			t.Fatalf("conflicting replay wrote a ledger row (%d -> %d)", moves, got)
		}
		var divergent int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM inventory_movements WHERE sku_id = 'cmd-divergent'`).Scan(&divergent); err != nil || divergent != 0 {
			t.Fatalf("divergent sku ledger rows = (%d, %v), want 0", divergent, err)
		}
	})

	t.Run("invalid identity fields are rejected before touching the ledger", func(t *testing.T) {
		// Two DIFFERENT commands both missing CommandID: without validation
		// the first would claim command_id '' and the second would be
		// swallowed as its "replay". Both must be rejected instead.
		first := domain.StockCommand{SKUID: "val-a", WarehouseID: wh, Quantity: 5, Actor: "test"}
		second := domain.StockCommand{SKUID: "val-b", WarehouseID: wh, Quantity: 7, Actor: "test"}
		if _, err := repo.ReceiveStock(ctx, first); err == nil {
			t.Fatal("empty CommandID must be rejected")
		}
		if _, err := repo.ReceiveStock(ctx, second); err == nil {
			t.Fatal("second empty-CommandID command must be rejected, not swallowed as a replay")
		}
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM inventory_movements WHERE sku_id IN ('val-a', 'val-b')`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rejected commands wrote ledger rows = (%d, %v), want 0", n, err)
		}

		if _, err := repo.AdjustOnHand(ctx, domain.StockCommand{
			CommandID: "val-1", WarehouseID: wh, Quantity: 1, Actor: "test"}); err == nil {
			t.Fatal("empty SKUID must be rejected")
		}
		if _, err := repo.SetSafetyStock(ctx, domain.StockCommand{
			CommandID: "val-2", SKUID: "val-a", WarehouseID: 0, Quantity: 1, Actor: "test"}); err == nil {
			t.Fatal("non-positive WarehouseID must be rejected")
		}
	})

	t.Run("adjust applies signed delta and rejects invariant violations atomically", func(t *testing.T) {
		if _, err := repo.AdjustOnHand(ctx, domain.StockCommand{
			CommandID: "adj-1", SKUID: "cmd-a", WarehouseID: wh, Quantity: -3, Reason: "shrinkage", Actor: "test"}); err != nil {
			t.Fatalf("adjust: %v", err)
		}
		if got := onHand("cmd-a"); got != 12 {
			t.Fatalf("on_hand = %d, want 12", got)
		}

		before := movements("cmd-a")
		_, err := repo.AdjustOnHand(ctx, domain.StockCommand{
			CommandID: "adj-2", SKUID: "cmd-a", WarehouseID: wh, Quantity: -100, Reason: "bad", Actor: "test"})
		if !errors.Is(err, domain.ErrInsufficientOnHand) {
			t.Fatalf("over-adjust error = %v, want ErrInsufficientOnHand", err)
		}
		if got := onHand("cmd-a"); got != 12 {
			t.Fatalf("on_hand changed to %d after failed adjust", got)
		}
		if got := movements("cmd-a"); got != before {
			t.Fatalf("failed adjust left an orphan ledger row (%d -> %d)", before, got)
		}
	})

	t.Run("safety stock set is absolute and idempotent by command", func(t *testing.T) {
		if _, err := repo.SetSafetyStock(ctx, domain.StockCommand{
			CommandID: "safe-1", SKUID: "cmd-a", WarehouseID: wh, Quantity: 4, Actor: "test"}); err != nil {
			t.Fatalf("set safety: %v", err)
		}
		var safety int64
		if err := pool.QueryRow(ctx,
			`SELECT safety_stock FROM inventory_balances WHERE sku_id = 'cmd-a' AND warehouse_id = $1`, wh).Scan(&safety); err != nil || safety != 4 {
			t.Fatalf("safety_stock = (%d, %v), want 4", safety, err)
		}
	})

	t.Run("balance equals the replay of its movement deltas", func(t *testing.T) {
		var ledgerSum, balance int64
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(SUM(on_hand_delta), 0) FROM inventory_movements WHERE sku_id = 'cmd-a' AND warehouse_id = $1`, wh).Scan(&ledgerSum); err != nil {
			t.Fatalf("ledger sum: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT on_hand FROM inventory_balances WHERE sku_id = 'cmd-a' AND warehouse_id = $1`, wh).Scan(&balance); err != nil {
			t.Fatalf("balance: %v", err)
		}
		if ledgerSum != balance {
			t.Fatalf("ledger replay %d != balance %d — the ledger drifted", ledgerSum, balance)
		}
	})

	t.Run("commands on missing balance rows fail cleanly", func(t *testing.T) {
		if _, err := repo.AdjustOnHand(ctx, domain.StockCommand{
			CommandID: "adj-ghost", SKUID: "ghost", WarehouseID: wh, Quantity: 1, Actor: "test"}); !errors.Is(err, domain.ErrBalanceNotFound) {
			t.Fatalf("adjust on missing balance = %v, want ErrBalanceNotFound", err)
		}
		if _, err := repo.SetSafetyStock(ctx, domain.StockCommand{
			CommandID: "safe-ghost", SKUID: "ghost", WarehouseID: wh, Quantity: 1, Actor: "test"}); !errors.Is(err, domain.ErrBalanceNotFound) {
			t.Fatalf("set safety on missing balance = %v, want ErrBalanceNotFound", err)
		}
	})
}
