//go:build integration

// Schema-constraint integration tests (RFC-0021 P1-2). They run the real
// migrations against a testcontainers Postgres and prove the DB backstops
// hold before any repository code exists: no negative balances, no
// reserved > on_hand, no zero-quantity lines, no duplicate movement
// commands, and the default warehouse is present. Run with:
//
//	go test -tags=integration ./internal/core/repository/...
package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	migrations "github.com/duynhlab/inventory-service/db/migrations"
	"github.com/duynhlab/pkg/migratex"
)

func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("inventory"),
		postgres.WithUsername("inventory"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := migratex.Run(migrations.FS, "sql", dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestInventorySchemaConstraints(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	var warehouseID int64
	t.Run("default warehouse seeded by migration", func(t *testing.T) {
		err := pool.QueryRow(ctx,
			`SELECT id FROM warehouses WHERE code = 'WH-DEFAULT' AND status = 'active'`).
			Scan(&warehouseID)
		if err != nil {
			t.Fatalf("default warehouse missing: %v", err)
		}
	})

	mustFail := func(t *testing.T, wantSubstr, query string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, query, args...)
		if err == nil {
			t.Fatalf("statement succeeded, want constraint violation: %s", query)
		}
		if wantSubstr != "" && !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("error %q does not mention %q", err, wantSubstr)
		}
	}

	t.Run("balance backstops", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved) VALUES ('sku-a', $1, 10, 2)`,
			warehouseID); err != nil {
			t.Fatalf("valid balance rejected: %v", err)
		}
		mustFail(t, "check", `INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand) VALUES ('sku-neg', $1, -1)`, warehouseID)
		mustFail(t, "check", `UPDATE inventory_balances SET reserved = 11 WHERE sku_id = 'sku-a' AND warehouse_id = $1`, warehouseID)
		mustFail(t, "check", `UPDATE inventory_balances SET on_hand = on_hand - 20 WHERE sku_id = 'sku-a' AND warehouse_id = $1`, warehouseID)
	})

	t.Run("reservation FSM vocabulary and idempotency keys", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO inventory_reservations (id, external_reference, request_hash) VALUES ('order-1', 'order-1', 'h1')`); err != nil {
			t.Fatalf("valid reservation rejected: %v", err)
		}
		mustFail(t, "duplicate", `INSERT INTO inventory_reservations (id, external_reference, request_hash) VALUES ('order-1b', 'order-1', 'h2')`)
		mustFail(t, "check", `INSERT INTO inventory_reservations (id, external_reference, request_hash, status) VALUES ('order-2', 'order-2', 'h', 'sold')`)

		if _, err := pool.Exec(ctx,
			`INSERT INTO inventory_reservation_lines (reservation_id, sku_id, warehouse_id, quantity) VALUES ('order-1', 'sku-a', $1, 2)`,
			warehouseID); err != nil {
			t.Fatalf("valid line rejected: %v", err)
		}
		mustFail(t, "check", `INSERT INTO inventory_reservation_lines (reservation_id, sku_id, warehouse_id, quantity) VALUES ('order-1', 'sku-b', $1, 0)`, warehouseID)
		mustFail(t, "foreign key", `INSERT INTO inventory_reservation_lines (reservation_id, sku_id, warehouse_id, quantity) VALUES ('ghost', 'sku-a', $1, 1)`, warehouseID)
	})

	t.Run("movement ledger command idempotency", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO inventory_movements (command_id, sku_id, warehouse_id, type, on_hand_delta) VALUES ('cmd-1', 'sku-a', $1, 'RECEIVE', 10)`,
			warehouseID); err != nil {
			t.Fatalf("valid movement rejected: %v", err)
		}
		mustFail(t, "duplicate", `INSERT INTO inventory_movements (command_id, sku_id, warehouse_id, type, on_hand_delta) VALUES ('cmd-1', 'sku-a', $1, 'RECEIVE', 10)`, warehouseID)
		mustFail(t, "check", `INSERT INTO inventory_movements (command_id, sku_id, warehouse_id, type) VALUES ('cmd-2', 'sku-a', $1, 'TELEPORT')`, warehouseID)
	})

	t.Run("no stored available column", func(t *testing.T) {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_name = 'inventory_balances' AND column_name IN ('available', 'available_to_promise')`).
			Scan(&n)
		if err != nil || n != 0 {
			t.Fatalf("derived availability must not be stored (n=%d, err=%v)", n, err)
		}
	})
}
