package domain

import (
	"context"
	"errors"
	"fmt"
)

// Backfill (RFC-0021 P2-2) migrates stock from product-service's database into
// inventory_balances so inventory can serve reads once the phase-2 cutover
// flips. It is READ-ONLY against product and WRITE against inventory.
//
// # Mapping (corrected — owner-approved)
//
// The backfill is correct ONLY at a DRAINED cutover: the RFC cutover runbook
// mandates draining every in-flight order (no active stock holds) before the
// backfill runs. At that point the mapping is a straight copy:
//
//	sku_id       = products.id (string)
//	on_hand      = products.stock_quantity
//	reserved     = 0
//	safety_stock = 0
//
// Product's stock_reservations ledger is DELIBERATELY NOT read. Product has no
// commit/sold state: ReserveStock decrements stock_quantity and inserts a
// 'reserved' row, but a SUCCESSFUL order NEVER clears that row — only the
// ReleaseStock compensation flips it to 'released'. So SUM(status='reserved')
// conflates in-flight holds with ALL completed sales; adding it back to on_hand
// would inflate on_hand/reserved permanently with phantom reserved that can
// never RELEASE or COMMIT. Reading only products.stock_quantity also removes
// the cross-table snapshot skew of a second read against a live database.
//
// Because there are no active holds at a drained cutover, reserved is 0 and the
// old "current_available == target_atp" invariant would be a tautology. It is
// replaced by a REAL guard: reject a negative stock_quantity (and defensively
// re-check the balance CHECKs) before writing anything.

// Mismatch reasons — why a SKU failed the guard and was excluded from writes.
const (
	// MismatchNegativeStock: products.stock_quantity was negative — impossible
	// under product's own CHECK, so it signals corrupt/torn data; refused
	// rather than written.
	MismatchNegativeStock = "negative_stock"
	// MismatchInvalidTarget: the reconstructed row would violate the balance
	// CHECKs (on_hand < 0, reserved < 0, or reserved > on_hand). Trivially
	// unreachable once stock is non-negative and reserved is 0, kept as defense
	// against a future mapping change.
	MismatchInvalidTarget = "invalid_target"
)

// Backfill run results (BackfillReport.Result).
const (
	ResultDryRun          = "dry_run_ok"
	ResultApplied         = "applied"
	ResultAbortedMismatch = "aborted_mismatch"
	ResultRefusedNonEmpty = "refused_non_empty"
	ResultNoProducts      = "no_products"
	ResultError           = "error"
)

// ErrBackfillMismatch is returned by RunBackfill when one or more SKUs fail the
// guard. No balances are written in that case.
var ErrBackfillMismatch = errors.New("backfill invariant mismatch")

// ErrBalancesExist is returned when --apply targets an inventory_balances that
// is already populated. The backfill is a drained pre-cutover one-shot and never
// overwrites existing balances: an absolute re-copy cannot preserve the
// append-only movement ledger invariant on_hand == SUM(on_hand_delta). To redo a
// run before cutover, truncate inventory_balances and the backfill movements,
// then run again (the only movements at that point are the backfill's own
// opening balances).
var ErrBalancesExist = errors.New("inventory_balances is not empty (truncate and re-run to redo)")

// ErrNoProducts is returned when the product read yields zero rows in apply mode.
// A backfill with nothing to migrate is treated as a loud failure (almost always
// a misdirected PRODUCT_DB_* connection) rather than a green no-op.
var ErrNoProducts = errors.New("product read returned zero rows")

// ProductRow is one product's stock snapshot read from product-service
// (products.id, products.stock_quantity). ProductID is the string sku_id.
type ProductRow struct {
	ProductID     string
	StockQuantity int64
}

// BalanceTarget is the reconstructed inventory_balances row for one SKU
// (warehouse is resolved once at write time, not carried per row).
type BalanceTarget struct {
	SKUID       string
	OnHand      int64
	Reserved    int64
	SafetyStock int64
}

// Mismatch names a SKU that failed the guard.
type Mismatch struct {
	SKUID  string
	OnHand int64
	Reason string
}

// BackfillPlan is the pure result of mapping product stock to inventory
// balances: the writable targets and the SKUs that failed the guard.
type BackfillPlan struct {
	Targets    []BalanceTarget
	Mismatches []Mismatch
}

// BuildBackfillPlan reconstructs inventory balances from product stock rows. It
// is pure and deterministic (no DB, no time): on_hand = stock_quantity,
// reserved = 0, safety_stock = 0, processed in input order. A negative
// stock_quantity (or any row that would violate the balance CHECKs) is refused
// as a mismatch instead of being written.
func BuildBackfillPlan(products []ProductRow) BackfillPlan {
	plan := BackfillPlan{
		Targets:    make([]BalanceTarget, 0, len(products)),
		Mismatches: make([]Mismatch, 0),
	}

	for _, p := range products {
		const reserved, safetyStock = 0, 0
		onHand := p.StockQuantity

		switch {
		case p.StockQuantity < 0:
			plan.Mismatches = append(plan.Mismatches, Mismatch{
				SKUID: p.ProductID, OnHand: onHand, Reason: MismatchNegativeStock,
			})
		case onHand < 0 || reserved < 0 || reserved > onHand:
			plan.Mismatches = append(plan.Mismatches, Mismatch{
				SKUID: p.ProductID, OnHand: onHand, Reason: MismatchInvalidTarget,
			})
		default:
			plan.Targets = append(plan.Targets, BalanceTarget{
				SKUID: p.ProductID, OnHand: onHand,
				Reserved: reserved, SafetyStock: safetyStock,
			})
		}
	}

	return plan
}

// ProductReader reads product-service stock READ-ONLY for the backfill. Only
// products.stock_quantity is needed — the reservation ledger is intentionally
// not read (see the package mapping notes).
type ProductReader interface {
	Products(ctx context.Context) ([]ProductRow, error)
}

// InventoryWriter applies the reconstructed balances to inventory storage.
type InventoryWriter interface {
	// DefaultWarehouseID resolves the WH-DEFAULT warehouse id.
	DefaultWarehouseID(ctx context.Context) (int64, error)
	// HasBalances reports whether inventory_balances already holds any row —
	// the apply-clobber guard for a pre-cutover one-shot.
	HasBalances(ctx context.Context) (bool, error)
	// UpsertBalances inserts the targets into inventory_balances for the given
	// warehouse within one transaction (RunBackfill has already refused a
	// non-empty table, so every row is a fresh INSERT), each paired with an
	// opening-balance RECEIVE movement (command_id backfill:<runID>:<sku>) so the
	// append-only ledger invariant on_hand == SUM(on_hand_delta) holds. It
	// returns how many SKUs were written; the whole batch commits atomically.
	UpsertBalances(ctx context.Context, warehouseID int64, runID string, targets []BalanceTarget) (int, error)
}

// BackfillOptions configures a run. RunID is an audit identifier supplied by
// the caller (flag/env) so the core stays free of time. Apply=false is the safe
// default: a dry-run reports and writes nothing. An already-populated
// inventory_balances is always refused (see ErrBalancesExist) — there is no
// overwrite option, because an absolute re-copy cannot preserve the append-only
// ledger.
type BackfillOptions struct {
	RunID string
	Apply bool
}

// BackfillReport is the audit record of a run.
type BackfillReport struct {
	RunID         string
	ProductCount  int
	TargetCount   int
	MismatchCount int
	Changed       int
	WarehouseID   int64
	Applied       bool
	Result        string
	Mismatches    []Mismatch
}

// RunBackfill reads product stock, reconstructs the target balances, guards
// them, and — only in apply mode with zero mismatches — upserts them. A
// mismatch aborts before any write in both modes (dry-run reports it too),
// returning ErrBackfillMismatch. In apply mode a zero-row product read fails
// loud (ErrNoProducts) and an already-populated inventory_balances is refused
// (ErrBalancesExist) — there is no overwrite path. Dry-run is the default and
// never writes.
func RunBackfill(ctx context.Context, r ProductReader, w InventoryWriter, opts BackfillOptions) (BackfillReport, error) {
	report := BackfillReport{RunID: opts.RunID}

	products, err := r.Products(ctx)
	if err != nil {
		report.Result = ResultError
		return report, fmt.Errorf("read products: %w", err)
	}

	plan := BuildBackfillPlan(products)
	report.ProductCount = len(products)
	report.TargetCount = len(plan.Targets)
	report.MismatchCount = len(plan.Mismatches)
	report.Mismatches = plan.Mismatches

	if len(plan.Mismatches) > 0 {
		report.Result = ResultAbortedMismatch
		return report, fmt.Errorf("%w: %d sku(s) failed the guard", ErrBackfillMismatch, len(plan.Mismatches))
	}

	// A zero-row read is almost always a misdirected PRODUCT_DB_* connection;
	// fail loud so a cutover operator never reads an empty migration as success.
	if len(products) == 0 {
		report.Result = ResultNoProducts
		return report, ErrNoProducts
	}

	if !opts.Apply {
		report.Result = ResultDryRun
		return report, nil
	}

	warehouseID, err := w.DefaultWarehouseID(ctx)
	if err != nil {
		report.Result = ResultError
		return report, fmt.Errorf("default warehouse: %w", err)
	}
	report.WarehouseID = warehouseID

	// The backfill is a one-shot into an empty table: an already-populated
	// inventory_balances is always refused (no overwrite path — absolute re-copy
	// can't preserve the append-only ledger; redo = truncate + re-run).
	populated, hasErr := w.HasBalances(ctx)
	if hasErr != nil {
		report.Result = ResultError
		return report, fmt.Errorf("check existing balances: %w", hasErr)
	}
	if populated {
		report.Result = ResultRefusedNonEmpty
		return report, ErrBalancesExist
	}

	changed, err := w.UpsertBalances(ctx, warehouseID, opts.RunID, plan.Targets)
	if err != nil {
		report.Result = ResultError
		return report, fmt.Errorf("upsert balances: %w", err)
	}

	report.Changed = changed
	report.Applied = true
	report.Result = ResultApplied
	return report, nil
}
