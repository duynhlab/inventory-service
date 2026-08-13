package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// AdminReadRepository serves the protected Backoffice list views
// (RFC-0023 slice A): balances, the movement ledger, and reservation
// headers. Read-only — every write still goes through StockCommandRepository
// or ReservationRepository.
type AdminReadRepository struct {
	pool *pgxpool.Pool
}

// NewAdminReadRepository creates the admin read repository.
func NewAdminReadRepository(pool *pgxpool.Pool) *AdminReadRepository {
	return &AdminReadRepository{pool: pool}
}

// listTotal runs the shared count query for a WHERE clause built by a lister.
func (r *AdminReadRepository) listTotal(ctx context.Context, table, where string, args []any) (int, error) {
	var total int
	//nolint:gosec // table and where are compile-time constants assembled below
	err := r.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+where, args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return total, nil
}

// balanceConditions translates a BalanceFilter into WHERE conditions.
// low-stock keeps rows whose derived ATP is at or below safety stock.
func balanceConditions(f domain.BalanceFilter, args *[]any) []string {
	conds := make([]string, 0, 3)
	if f.SKUID != "" {
		*args = append(*args, f.SKUID)
		conds = append(conds, fmt.Sprintf("sku_id = $%d", len(*args)))
	}
	if f.WarehouseID > 0 {
		*args = append(*args, f.WarehouseID)
		conds = append(conds, fmt.Sprintf("warehouse_id = $%d", len(*args)))
	}
	if f.LowStockOnly {
		conds = append(conds, "(on_hand - reserved) <= safety_stock")
	}
	return conds
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// ListBalances returns one page of balance rows (stable sku, warehouse
// order) plus the unpaged total for the same filter.
func (r *AdminReadRepository) ListBalances(ctx context.Context, f domain.BalanceFilter, limit, offset int) ([]domain.BalanceView, int, error) {
	args := make([]any, 0, 4)
	where := whereClause(balanceConditions(f, &args))

	total, err := r.listTotal(ctx, "inventory_balances", where, args)
	if err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`
		SELECT sku_id, warehouse_id, on_hand, reserved, safety_stock,
		       on_hand - reserved AS atp, updated_at
		FROM inventory_balances%s
		ORDER BY sku_id, warehouse_id
		LIMIT $%d OFFSET $%d`, where, len(args)+1, len(args)+2)
	rows, err := r.pool.Query(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list balances: %w", err)
	}
	items, err := scanBalances(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// SKUBalances returns every warehouse row for one SKU (empty slice when the
// SKU is untracked — the web layer turns that into 404).
func (r *AdminReadRepository) SKUBalances(ctx context.Context, skuID string) ([]domain.BalanceView, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT sku_id, warehouse_id, on_hand, reserved, safety_stock,
		       on_hand - reserved AS atp, updated_at
		FROM inventory_balances
		WHERE sku_id = $1
		ORDER BY warehouse_id`, skuID)
	if err != nil {
		return nil, fmt.Errorf("sku balances: %w", err)
	}
	return scanBalances(rows)
}

func scanBalances(rows pgx.Rows) ([]domain.BalanceView, error) {
	defer rows.Close()
	items := make([]domain.BalanceView, 0)
	for rows.Next() {
		var b domain.BalanceView
		var updatedAt time.Time
		if err := rows.Scan(&b.SKUID, &b.WarehouseID, &b.OnHand, &b.Reserved,
			&b.SafetyStock, &b.ATP, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan balance: %w", err)
		}
		b.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		items = append(items, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate balances: %w", err)
	}
	return items, nil
}

// ListMovements returns one page of the append-only ledger, newest first,
// plus the unpaged total for the same filter.
func (r *AdminReadRepository) ListMovements(ctx context.Context, f domain.MovementFilter, limit, offset int) ([]domain.MovementView, int, error) {
	args := make([]any, 0, 4)
	conds := make([]string, 0, 2)
	if f.SKUID != "" {
		args = append(args, f.SKUID)
		conds = append(conds, fmt.Sprintf("sku_id = $%d", len(args)))
	}
	if f.WarehouseID > 0 {
		args = append(args, f.WarehouseID)
		conds = append(conds, fmt.Sprintf("warehouse_id = $%d", len(args)))
	}
	where := whereClause(conds)

	total, err := r.listTotal(ctx, "inventory_movements", where, args)
	if err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`
		SELECT id, command_id, sku_id, warehouse_id, type,
		       on_hand_delta, reserved_delta,
		       COALESCE(reference_type, ''), COALESCE(reference_id, ''),
		       COALESCE(reason, ''), COALESCE(actor, ''), created_at
		FROM inventory_movements%s
		ORDER BY id DESC
		LIMIT $%d OFFSET $%d`, where, len(args)+1, len(args)+2)
	rows, err := r.pool.Query(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list movements: %w", err)
	}
	defer rows.Close()

	items := make([]domain.MovementView, 0)
	for rows.Next() {
		var m domain.MovementView
		var createdAt time.Time
		if err := rows.Scan(&m.ID, &m.CommandID, &m.SKUID, &m.WarehouseID, &m.Type,
			&m.OnHandDelta, &m.ReservedDelta, &m.ReferenceType, &m.ReferenceID,
			&m.Reason, &m.Actor, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scan movement: %w", err)
		}
		m.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate movements: %w", err)
	}
	return items, total, nil
}

// ListReservations returns one page of reservation headers, newest first,
// plus the unpaged total. status narrows to one lifecycle state when set.
func (r *AdminReadRepository) ListReservations(ctx context.Context, status string, limit, offset int) ([]domain.ReservationView, int, error) {
	args := make([]any, 0, 3)
	conds := make([]string, 0, 1)
	if status != "" {
		args = append(args, status)
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	where := whereClause(conds)

	total, err := r.listTotal(ctx, "inventory_reservations", where, args)
	if err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`
		SELECT id, external_reference, status, expires_at, created_at, updated_at
		FROM inventory_reservations%s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d`, where, len(args)+1, len(args)+2)
	rows, err := r.pool.Query(ctx, query, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()

	items := make([]domain.ReservationView, 0)
	for rows.Next() {
		var v domain.ReservationView
		var createdAt, updatedAt time.Time
		var expiresAt *time.Time
		if err := rows.Scan(&v.ID, &v.OrderID, &v.Status, &expiresAt,
			&createdAt, &updatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan reservation: %w", err)
		}
		if expiresAt != nil {
			v.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
		}
		v.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		v.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate reservations: %w", err)
	}
	return items, total, nil
}
