package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AvailabilityRepository serves the read-side availability queries (RFC-0021
// P1-4). Available-to-promise is always derived in SQL —
// GREATEST(0, on_hand - reserved - safety_stock) — never stored, so it cannot
// drift from its inputs (see migration 000002). Only ACTIVE warehouses count:
// stock parked in an inactive warehouse exists physically but is not
// promisable.
type AvailabilityRepository struct {
	db *pgxpool.Pool
}

// NewAvailabilityRepository creates a pgx-backed availability repository.
func NewAvailabilityRepository(db *pgxpool.Pool) *AvailabilityRepository {
	return &AvailabilityRepository{db: db}
}

// BatchATP returns available-to-promise per SKU, summed over active
// warehouses. The per-warehouse floor at 0 happens before the sum so a
// warehouse oversubscribed by safety stock cannot cancel out promisable units
// held elsewhere. SKUs with no balance row in any active warehouse are simply
// absent from the map — the caller pairs this with TrackedSKUs to decide
// whether absence means OUT_OF_STOCK (tracked, nothing promisable) or
// UNKNOWN (untracked).
func (r *AvailabilityRepository) BatchATP(ctx context.Context, skuIDs []string) (map[string]int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT b.sku_id, SUM(GREATEST(0, b.on_hand - b.reserved - b.safety_stock))::bigint
		FROM inventory_balances b
		JOIN warehouses w ON w.id = b.warehouse_id AND w.status = 'active'
		WHERE b.sku_id = ANY($1)
		GROUP BY b.sku_id`, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("batch atp: %w", err)
	}
	defer rows.Close()

	atp := make(map[string]int64, len(skuIDs))
	for rows.Next() {
		var skuID string
		var n int64
		if err := rows.Scan(&skuID, &n); err != nil {
			return nil, fmt.Errorf("batch atp scan: %w", err)
		}
		atp[skuID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("batch atp rows: %w", err)
	}
	return atp, nil
}

// ActiveWarehouseBalances returns per-warehouse ATP for the given SKUs
// (warehouse_id → sku_id → ATP) plus every active warehouse id in ascending
// order — including warehouses with no balance rows for these SKUs. The
// whole-basket check needs the per-warehouse split because v1 fulfills an
// order from one warehouse (a basket can be unfulfillable even when the
// summed ATP suffices), and it needs the full active list because the
// shortage baseline is the lowest-id ACTIVE warehouse, not the lowest one
// that happens to hold rows. A (warehouse, sku) pair with no balance row is
// absent from the inner map.
func (r *AvailabilityRepository) ActiveWarehouseBalances(ctx context.Context, skuIDs []string) (map[int64]map[string]int64, []int64, error) {
	idRows, err := r.db.Query(ctx, `
		SELECT id FROM warehouses WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("active warehouse ids: %w", err)
	}
	defer idRows.Close()

	var activeIDs []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("active warehouse ids scan: %w", err)
		}
		activeIDs = append(activeIDs, id)
	}
	if err := idRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("active warehouse ids rows: %w", err)
	}

	rows, err := r.db.Query(ctx, `
		SELECT b.warehouse_id, b.sku_id, GREATEST(0, b.on_hand - b.reserved - b.safety_stock)
		FROM inventory_balances b
		JOIN warehouses w ON w.id = b.warehouse_id AND w.status = 'active'
		WHERE b.sku_id = ANY($1)`, skuIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("active warehouse balances: %w", err)
	}
	defer rows.Close()

	balances := make(map[int64]map[string]int64)
	for rows.Next() {
		var warehouseID int64
		var skuID string
		var atp int64
		if err := rows.Scan(&warehouseID, &skuID, &atp); err != nil {
			return nil, nil, fmt.Errorf("active warehouse balances scan: %w", err)
		}
		if balances[warehouseID] == nil {
			balances[warehouseID] = make(map[string]int64)
		}
		balances[warehouseID][skuID] = atp
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("active warehouse balances rows: %w", err)
	}
	return balances, activeIDs, nil
}

// TrackedSKUs reports which of the given SKUs have a balance row in ANY
// warehouse, active or not. It separates "inventory knows this SKU but
// nothing is promisable" (tracked → OUT_OF_STOCK) from "inventory has never
// heard of it" (untracked → UNKNOWN): a deactivated warehouse's exclusive
// SKUs are genuinely un-promisable, not unknowable.
func (r *AvailabilityRepository) TrackedSKUs(ctx context.Context, skuIDs []string) (map[string]bool, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT sku_id FROM inventory_balances WHERE sku_id = ANY($1)`, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("tracked skus: %w", err)
	}
	defer rows.Close()

	tracked := make(map[string]bool, len(skuIDs))
	for rows.Next() {
		var skuID string
		if err := rows.Scan(&skuID); err != nil {
			return nil, fmt.Errorf("tracked skus scan: %w", err)
		}
		tracked[skuID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracked skus rows: %w", err)
	}
	return tracked, nil
}
