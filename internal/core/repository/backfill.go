package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// defaultWarehouseCode is the single warehouse every environment provisions in
// the schema migration (RFC-0021 P1-2). The backfill writes all balances there.
const defaultWarehouseCode = "WH-DEFAULT"

// BackfillRepository is the pgx-backed domain.InventoryWriter for the phase-2
// backfill (RFC-0021 P2-2). It writes inventory_balances only; the product
// read side lives outside this repository (it targets a different database).
type BackfillRepository struct {
	pool *pgxpool.Pool
}

// NewBackfillRepository creates a backfill writer over the inventory pool.
func NewBackfillRepository(pool *pgxpool.Pool) *BackfillRepository {
	return &BackfillRepository{pool: pool}
}

var _ domain.InventoryWriter = (*BackfillRepository)(nil)

// DefaultWarehouseID resolves the WH-DEFAULT warehouse. Its absence is a
// deployment error (the migration seeds it), not a data condition.
func (r *BackfillRepository) DefaultWarehouseID(ctx context.Context) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM warehouses WHERE code = $1`, defaultWarehouseCode).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("default warehouse %q not found (run migrations)", defaultWarehouseCode)
		}
		return 0, fmt.Errorf("resolve default warehouse: %w", err)
	}
	return id, nil
}

// HasBalances reports whether inventory_balances holds any row — the
// apply-clobber guard: the backfill is a pre-cutover one-shot and must not
// silently overwrite live balances on a post-cutover re-run.
func (r *BackfillRepository) HasBalances(ctx context.Context) (bool, error) {
	// The caller (domain.RunBackfill) adds the "check existing balances"
	// context, so the raw query error is returned with only local detail.
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inventory_balances)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query inventory_balances existence: %w", err)
	}
	return exists, nil
}

// UpsertBalances inserts every target into inventory_balances for warehouseID in
// ONE transaction. RunBackfill has already refused a non-empty table, so every
// SKU is a fresh INSERT: each balance is paired with an opening-balance RECEIVE
// movement (command_id backfill:<runID>:<sku>, UNIQUE) carrying on_hand_delta =
// on_hand, so the append-only ledger invariant on_hand == SUM(on_hand_delta)
// holds by construction (exactly one movement per SKU, delta == balance). There
// is no overwrite path — a duplicate SKU or a re-run into a populated table hits
// a unique/CHECK violation and rolls the whole batch back rather than silently
// clobbering. The batch commits atomically: a single bad row rolls back the
// entire run, never a partial write.
func (r *BackfillRepository) UpsertBalances(ctx context.Context, warehouseID int64, runID string, targets []domain.BalanceTarget) (int, error) {
	if len(targets) == 0 {
		return 0, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin backfill: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	changed := 0
	for _, t := range targets {
		cmdID := fmt.Sprintf("backfill:%s:%s", runID, t.SKUID)

		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_movements
				(command_id, sku_id, warehouse_id, type, on_hand_delta, reserved_delta, reference_type)
			VALUES ($1, $2, $3, 'RECEIVE', $4, 0, 'backfill')`,
			cmdID, t.SKUID, warehouseID, t.OnHand); err != nil {
			return 0, fmt.Errorf("insert backfill movement %s: %w", cmdID, err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
			VALUES ($1, $2, $3, $4, $5)`,
			t.SKUID, warehouseID, t.OnHand, t.Reserved, t.SafetyStock); err != nil {
			return 0, fmt.Errorf("insert balance %s: %w", t.SKUID, err)
		}
		changed++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit backfill: %w", err)
	}
	return changed, nil
}
