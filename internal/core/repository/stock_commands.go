package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// StockCommandRepository is the pgx-backed domain.StockCommander. Every
// command runs in one transaction: inserting the movement row claims the
// command_id (UNIQUE), and the balance change commits atomically with it —
// a lost response can be retried without double-applying, and a failed
// balance change leaves no orphan ledger row.
type StockCommandRepository struct {
	pool *pgxpool.Pool
}

func NewStockCommandRepository(pool *pgxpool.Pool) *StockCommandRepository {
	return &StockCommandRepository{pool: pool}
}

var _ domain.StockCommander = (*StockCommandRepository)(nil)

// claimCommand inserts the movement row for cmd. It returns false when the
// command_id already exists (idempotent replay — the whole command becomes a
// no-op) and the transaction should be rolled back without error.
func claimCommand(ctx context.Context, tx pgx.Tx, cmd domain.StockCommand, movementType string, onHandDelta, reservedDelta int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO inventory_movements
			(command_id, sku_id, warehouse_id, type, on_hand_delta, reserved_delta, reference_type, reference_id, reason)
		VALUES ($1, $2, $3, $4, $5, $6, 'admin', $7, $8)
		ON CONFLICT (command_id) DO NOTHING`,
		cmd.CommandID, cmd.SKUID, cmd.WarehouseID, movementType, onHandDelta, reservedDelta, cmd.Actor, cmd.Reason)
	if err != nil {
		return false, fmt.Errorf("claim command %s: %w", cmd.CommandID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// run wraps the claim + apply steps in a transaction and normalizes the
// replay path (claimed=false → rollback, applied=false, nil error).
func (r *StockCommandRepository) run(
	ctx context.Context,
	cmd domain.StockCommand,
	movementType string,
	onHandDelta, reservedDelta int64,
	apply func(pgx.Tx) error,
) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	claimed, err := claimCommand(ctx, tx, cmd, movementType, onHandDelta, reservedDelta)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil // idempotent replay
	}
	if err := apply(tx); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit %s: %w", cmd.CommandID, err)
	}
	return true, nil
}

// checkViolation maps a Postgres CHECK failure onto the business error — the
// schema is the invariant backstop, the error name is the contract.
func checkViolation(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" { // check_violation
		return domain.ErrInsufficientOnHand
	}
	return err
}

func (r *StockCommandRepository) ReceiveStock(ctx context.Context, cmd domain.StockCommand) (bool, error) {
	if cmd.Quantity <= 0 {
		return false, fmt.Errorf("receive quantity must be > 0, got %d", cmd.Quantity)
	}
	return r.run(ctx, cmd, domain.MovementReceive, cmd.Quantity, 0, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand)
			VALUES ($1, $2, $3)
			ON CONFLICT (sku_id, warehouse_id)
			DO UPDATE SET on_hand = inventory_balances.on_hand + EXCLUDED.on_hand,
			              version = inventory_balances.version + 1,
			              updated_at = now()`,
			cmd.SKUID, cmd.WarehouseID, cmd.Quantity)
		if err != nil {
			return fmt.Errorf("receive stock: %w", err)
		}
		return nil
	})
}

func (r *StockCommandRepository) AdjustOnHand(ctx context.Context, cmd domain.StockCommand) (bool, error) {
	if cmd.Quantity == 0 {
		return false, errors.New("adjust delta must be non-zero")
	}
	return r.run(ctx, cmd, domain.MovementAdjust, cmd.Quantity, 0, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_balances
			SET on_hand = on_hand + $3, version = version + 1, updated_at = now()
			WHERE sku_id = $1 AND warehouse_id = $2`,
			cmd.SKUID, cmd.WarehouseID, cmd.Quantity)
		if err != nil {
			return fmt.Errorf("adjust on_hand: %w", checkViolation(err))
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("adjust on_hand: no balance row for sku %s in warehouse %d", cmd.SKUID, cmd.WarehouseID)
		}
		return nil
	})
}

func (r *StockCommandRepository) SetSafetyStock(ctx context.Context, cmd domain.StockCommand) (bool, error) {
	if cmd.Quantity < 0 {
		return false, fmt.Errorf("safety stock must be >= 0, got %d", cmd.Quantity)
	}
	return r.run(ctx, cmd, domain.MovementSetSafetyStock, 0, 0, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_balances
			SET safety_stock = $3, version = version + 1, updated_at = now()
			WHERE sku_id = $1 AND warehouse_id = $2`,
			cmd.SKUID, cmd.WarehouseID, cmd.Quantity)
		if err != nil {
			return fmt.Errorf("set safety stock: %w", checkViolation(err))
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("set safety stock: no balance row for sku %s in warehouse %d", cmd.SKUID, cmd.WarehouseID)
		}
		return nil
	})
}
