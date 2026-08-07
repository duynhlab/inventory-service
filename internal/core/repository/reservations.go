package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// ReservationRepository is the pgx-backed reservation command store
// (RFC-0021 P1-5). Every command runs in one transaction: Reserve claims the
// reservation id by inserting the header row (the PK is the idempotency
// key), then allocates and applies under FOR UPDATE locks — any shortage
// rolls the WHOLE transaction back, claim included, so a failed reservation
// leaves no trace. Release/Commit serialize on the header row lock, making
// the RESERVED→RELEASED/COMMITTED transition apply exactly once.
type ReservationRepository struct {
	pool *pgxpool.Pool
}

// NewReservationRepository creates the pgx-backed reservation repository.
func NewReservationRepository(pool *pgxpool.Pool) *ReservationRepository {
	return &ReservationRepository{pool: pool}
}

// querier abstracts pgx.Tx and *pgxpool.Pool for the shared readers.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// translateConcurrency maps Postgres concurrency aborts — serialization
// failure (40001), deadlock (40P01), and lock_timeout expiry (55P03, see the
// pool's lock_timeout runtime param) — onto the retryable domain error;
// everything else passes through unchanged.
func translateConcurrency(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) &&
		(pgErr.Code == "40001" || pgErr.Code == "40P01" || pgErr.Code == "55P03") {
		return fmt.Errorf("%w: %s", domain.ErrConcurrencyConflict, pgErr.Code)
	}
	return err
}

// Reserve places an all-or-nothing hold for req. Idempotent by reservation
// id + canonical request hash (computed server-side, never trusted from the
// caller): a replay returns the original result; a divergent payload under
// the same id, or a different id reusing the order id, returns
// domain.ErrIdempotencyConflict. A basket no single active warehouse can
// fulfill returns domain.ErrInsufficientStock (as *InsufficientStockError).
func (r *ReservationRepository) Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	var zero domain.ReservationResult
	hash := domain.CanonicalHash(req.Items, req.DestinationRegion)

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return zero, fmt.Errorf("begin reserve: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	claimed, err := claimReservation(ctx, tx, req, hash)
	if err != nil {
		if !errors.Is(err, errOrderRefTaken) {
			return zero, err
		}
		// The claim died on the external_reference unique index while
		// ON CONFLICT (id) stayed silent: either this exact request lost the
		// race to its twin (same id — replay) or a different reservation id
		// holds the order (conflict). The 23505 aborted the transaction, so
		// resolve against the committed state outside it. Order-id squatting
		// (guessing another order's id to poison its reservation) is an
		// accepted risk while callers sit behind the cluster NetworkPolicy
		// fence; RFC-0020 internal mTLS adds caller identity to close it.
		_ = tx.Rollback(ctx)
		res, rerr := replayReservation(ctx, r.pool, req.ID, req.OrderID, hash)
		if errors.Is(rerr, pgx.ErrNoRows) {
			return zero, fmt.Errorf("order %s already reserved under another reservation id: %w",
				req.OrderID, domain.ErrIdempotencyConflict)
		}
		return res, rerr
	}
	if !claimed {
		// The id is taken and — because the unique-index wait on the claim
		// INSERT only ends when the winner has committed or aborted — the
		// committed row is visible to this statement's snapshot.
		return replayReservation(ctx, tx, req.ID, req.OrderID, hash)
	}

	allocations, err := allocateReservation(ctx, tx, req.Items)
	if err != nil {
		return zero, err // rollback drops the claim with it
	}
	if err := applyReserve(ctx, tx, req.ID, allocations); err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, fmt.Errorf("commit reserve %s: %w", req.ID, translateConcurrency(err))
	}
	return domain.ReservationResult{
		ID:          req.ID,
		Status:      domain.ReservationReserved,
		Allocations: allocations,
	}, nil
}

// errOrderRefTaken marks a 23505 on the claim INSERT. With ON CONFLICT (id)
// absorbing the primary key, the only index that can still raise is the
// external_reference UNIQUE — a row (same id or another) already carries this
// order id. The error aborts the transaction; Reserve resolves it outside.
var errOrderRefTaken = errors.New("order external_reference already claimed")

// claimReservation inserts the header row, claiming the reservation id. It
// returns false when the id already exists (replay-or-conflict, resolved by
// the caller inside the same transaction).
func claimReservation(ctx context.Context, tx pgx.Tx, req domain.ReservationRequest, hash string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO inventory_reservations (id, external_reference, request_hash, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::timestamptz)
		ON CONFLICT (id) DO NOTHING`,
		req.ID, req.OrderID, hash, req.ExpiresAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return false, fmt.Errorf("claim reservation %s: %w", req.ID, errOrderRefTaken)
		}
		return false, fmt.Errorf("claim reservation %s: %w", req.ID, translateConcurrency(err))
	}
	return tag.RowsAffected() == 1, nil
}

// replayReservation resolves a taken reservation id: same canonical hash AND
// same order id → return the committed result (idempotent replay); a
// different hash or a different order id → the caller sent a divergent
// request under a used id (the hash covers items+destination only, so the
// order reference must be compared explicitly). A missing row propagates as
// a wrapped pgx.ErrNoRows for the caller to classify.
func replayReservation(ctx context.Context, q querier, id, orderID, hash string) (domain.ReservationResult, error) {
	var zero domain.ReservationResult
	var storedRef, storedHash, status string
	err := q.QueryRow(ctx, `
		SELECT external_reference, request_hash, status FROM inventory_reservations WHERE id = $1`, id).
		Scan(&storedRef, &storedHash, &status)
	if err != nil {
		return zero, fmt.Errorf("load claimed reservation %s: %w", id, translateConcurrency(err))
	}
	if storedHash != hash || storedRef != orderID {
		return zero, fmt.Errorf("reservation %s: %w", id, domain.ErrIdempotencyConflict)
	}
	allocations, err := loadReservationLines(ctx, q, id)
	if err != nil {
		return zero, err
	}
	return domain.ReservationResult{
		ID:          id,
		Status:      status,
		Allocations: allocations,
		Replayed:    true,
	}, nil
}

// allocateReservation picks the lowest-id ACTIVE warehouse whose
// per-warehouse ATP satisfies EVERY line (v1 fulfills a whole order from one
// warehouse), locks that warehouse's balance rows in (warehouse_id, sku_id)
// order, and re-validates ATP under the locks — the unlocked scan is only a
// hint; the locked read is the correctness gate. A requested SKU with no
// balance row in the chosen warehouse counts as ATP 0. When no warehouse
// fulfills, shortages are computed against the lowest-id active warehouse —
// the same baseline rule as CheckAvailability.
func allocateReservation(ctx context.Context, tx pgx.Tx, items []domain.Line) ([]domain.Allocation, error) {
	byWarehouse, activeIDs, err := activeATPSnapshot(ctx, tx, items)
	if err != nil {
		return nil, err
	}

	chosen, found := int64(0), false
	for _, wh := range activeIDs {
		if fulfillsLines(byWarehouse[wh], items) {
			chosen, found = wh, true
			break
		}
	}
	if !found {
		var base map[string]int64
		if len(activeIDs) > 0 {
			base = byWarehouse[activeIDs[0]]
		}
		return nil, classifyReservationFailure(ctx, tx, items, shortagesAgainst(base, items))
	}

	locked, err := lockWarehouseATP(ctx, tx, chosen, items)
	if err != nil {
		return nil, err
	}
	if short := shortagesAgainst(locked, items); len(short) > 0 {
		return nil, classifyReservationFailure(ctx, tx, items, short)
	}

	allocations := make([]domain.Allocation, 0, len(items))
	for _, it := range items {
		allocations = append(allocations, domain.Allocation{
			SKUID: it.SKUID, WarehouseID: chosen, Quantity: it.Quantity,
		})
	}
	return allocations, nil
}

// activeATPSnapshot reads per-warehouse ATP for the requested SKUs plus every
// active warehouse id ascending — the same unlocked derivation the
// availability reads use, but inside this transaction.
func activeATPSnapshot(ctx context.Context, tx pgx.Tx, items []domain.Line) (map[int64]map[string]int64, []int64, error) {
	idRows, err := tx.Query(ctx, `SELECT id FROM warehouses WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("active warehouse ids: %w", translateConcurrency(err))
	}
	activeIDs, err := pgx.CollectRows(idRows, pgx.RowTo[int64])
	if err != nil {
		return nil, nil, fmt.Errorf("active warehouse ids scan: %w", translateConcurrency(err))
	}

	rows, err := tx.Query(ctx, `
		SELECT b.warehouse_id, b.sku_id, GREATEST(0, b.on_hand - b.reserved - b.safety_stock)
		FROM inventory_balances b
		JOIN warehouses w ON w.id = b.warehouse_id AND w.status = 'active'
		WHERE b.sku_id = ANY($1)`, skuIDsOf(items))
	if err != nil {
		return nil, nil, fmt.Errorf("warehouse atp snapshot: %w", translateConcurrency(err))
	}
	defer rows.Close()

	byWarehouse := make(map[int64]map[string]int64)
	for rows.Next() {
		var wh int64
		var sku string
		var atp int64
		if err := rows.Scan(&wh, &sku, &atp); err != nil {
			return nil, nil, fmt.Errorf("warehouse atp scan: %w", err)
		}
		if byWarehouse[wh] == nil {
			byWarehouse[wh] = make(map[string]int64)
		}
		byWarehouse[wh][sku] = atp
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("warehouse atp rows: %w", translateConcurrency(err))
	}
	return byWarehouse, activeIDs, nil
}

// lockWarehouseATP locks the chosen warehouse's balance rows for the
// requested SKUs — ordered by (warehouse_id, sku_id) so concurrent
// reservations acquire locks in one global order — and returns ATP as read
// under the locks.
func lockWarehouseATP(ctx context.Context, tx pgx.Tx, warehouseID int64, items []domain.Line) (map[string]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT sku_id, GREATEST(0, on_hand - reserved - safety_stock)
		FROM inventory_balances
		WHERE warehouse_id = $1 AND sku_id = ANY($2)
		ORDER BY warehouse_id, sku_id
		FOR UPDATE`, warehouseID, skuIDsOf(items))
	if err != nil {
		return nil, fmt.Errorf("lock balances wh=%d: %w", warehouseID, translateConcurrency(err))
	}
	defer rows.Close()

	atp := make(map[string]int64, len(items))
	for rows.Next() {
		var sku string
		var n int64
		if err := rows.Scan(&sku, &n); err != nil {
			return nil, fmt.Errorf("locked atp scan: %w", err)
		}
		atp[sku] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("locked atp rows: %w", translateConcurrency(err))
	}
	return atp, nil
}

// applyReserve applies the allocation: bump reserved per balance row, insert
// the reservation lines, and append one RESERVE movement per line with
// command_id `res:<reservation_id>:<sku>` so a whole-reservation retry can
// never double-write a line's ledger entry.
func applyReserve(ctx context.Context, tx pgx.Tx, reservationID string, allocations []domain.Allocation) error {
	for _, a := range allocations {
		if _, err := tx.Exec(ctx, `
			UPDATE inventory_balances
			SET reserved = reserved + $3, version = version + 1, updated_at = now()
			WHERE sku_id = $1 AND warehouse_id = $2`,
			a.SKUID, a.WarehouseID, a.Quantity); err != nil {
			return fmt.Errorf("reserve balance %s: %w", a.SKUID, translateConcurrency(err))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_reservation_lines (reservation_id, sku_id, warehouse_id, quantity)
			VALUES ($1, $2, $3, $4)`,
			reservationID, a.SKUID, a.WarehouseID, a.Quantity); err != nil {
			return fmt.Errorf("insert reservation line %s: %w", a.SKUID, translateConcurrency(err))
		}
		if err := insertReservationMovement(ctx, tx, "res:"+reservationID+":"+a.SKUID,
			a, domain.MovementReserve, 0, a.Quantity, reservationID, ""); err != nil {
			return err
		}
	}
	return nil
}

// insertReservationMovement appends one reservation-driven ledger row. actor
// stays NULL — the originator is the workflow identified by
// reference_type/reference_id; reason is NULL unless provided (Release).
func insertReservationMovement(ctx context.Context, tx pgx.Tx, commandID string,
	a domain.Allocation, movementType string, onHandDelta, reservedDelta int64,
	reservationID, reason string,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory_movements
			(command_id, sku_id, warehouse_id, type, on_hand_delta, reserved_delta,
			 reference_type, reference_id, reason)
		VALUES ($1, $2, $3, $4, $5, $6, 'reservation', $7, NULLIF($8, ''))`,
		commandID, a.SKUID, a.WarehouseID, movementType, onHandDelta, reservedDelta,
		reservationID, reason); err != nil {
		return fmt.Errorf("insert %s movement %s: %w", movementType, commandID, translateConcurrency(err))
	}
	return nil
}

// Release returns a reservation's stock (saga compensation). A missing or
// already-released reservation is an idempotent no-op success; a committed or
// expired one returns domain.ErrInvalidTransition — a sale is undone by a
// return movement, never by release.
func (r *ReservationRepository) Release(ctx context.Context, id, reason string) (string, error) {
	return r.transition(ctx, id, domain.ReservationReleased,
		func(status string) (string, error) {
			switch status {
			case "":
				// Missing row: compensation no-op per the proto contract. A
				// saga can compensate before its Reserve ever landed
				// (Release-before-Reserve); if that Reserve lands later it
				// creates an orphaned hold this call never saw — the
				// order-domain reconciler (RFC-0021 P3-5) owns that seam,
				// not this repository.
				return domain.ReservationReleased, nil
			case domain.ReservationReleased:
				return domain.ReservationReleased, nil
			case domain.ReservationCommitted, domain.ReservationExpired:
				return "", fmt.Errorf("release %s from %s: %w", id, status, domain.ErrInvalidTransition)
			}
			return "", nil // reserved: proceed
		},
		func(tx pgx.Tx, a domain.Allocation) error {
			tag, err := tx.Exec(ctx, `
				UPDATE inventory_balances
				SET reserved = reserved - $3, version = version + 1, updated_at = now()
				WHERE sku_id = $1 AND warehouse_id = $2`,
				a.SKUID, a.WarehouseID, a.Quantity)
			if err != nil {
				return fmt.Errorf("release balance %s: %w", a.SKUID, translateConcurrency(err))
			}
			if tag.RowsAffected() != 1 {
				// Fail closed: a vanished balance row must abort the whole
				// transition — the FSM may never flip without moving stock.
				return fmt.Errorf("release balance %s/%d: %d rows updated, want 1",
					a.SKUID, a.WarehouseID, tag.RowsAffected())
			}
			return insertReservationMovement(ctx, tx, "rel:"+id+":"+a.SKUID,
				a, domain.MovementRelease, 0, -a.Quantity, id, reason)
		})
}

// Commit converts a reservation into a sale: on_hand and reserved both
// decrease and a SALE_COMMITTED movement is recorded per line. Committing a
// committed reservation is an idempotent replay; a released or expired one
// returns domain.ErrInvalidTransition; a missing id returns
// domain.ErrReservationNotFound — a confirmed order must converge to
// COMMITTED, so the caller has to know the hold never existed.
func (r *ReservationRepository) Commit(ctx context.Context, id string) (string, error) {
	return r.transition(ctx, id, domain.ReservationCommitted,
		func(status string) (string, error) {
			switch status {
			case "":
				return "", fmt.Errorf("commit %s: %w", id, domain.ErrReservationNotFound)
			case domain.ReservationCommitted: // replay
				return domain.ReservationCommitted, nil
			case domain.ReservationReleased, domain.ReservationExpired:
				return "", fmt.Errorf("commit %s from %s: %w", id, status, domain.ErrInvalidTransition)
			}
			return "", nil // reserved: proceed
		},
		func(tx pgx.Tx, a domain.Allocation) error {
			tag, err := tx.Exec(ctx, `
				UPDATE inventory_balances
				SET on_hand = on_hand - $3, reserved = reserved - $3, version = version + 1, updated_at = now()
				WHERE sku_id = $1 AND warehouse_id = $2`,
				a.SKUID, a.WarehouseID, a.Quantity)
			if err != nil {
				return fmt.Errorf("commit balance %s: %w", a.SKUID, translateConcurrency(err))
			}
			if tag.RowsAffected() != 1 {
				// Fail closed: a vanished balance row must abort the whole
				// transition — the FSM may never flip without moving stock.
				return fmt.Errorf("commit balance %s/%d: %d rows updated, want 1",
					a.SKUID, a.WarehouseID, tag.RowsAffected())
			}
			return insertReservationMovement(ctx, tx, "cmt:"+id+":"+a.SKUID,
				a, domain.MovementSaleCommitted, -a.Quantity, -a.Quantity, id, "")
		})
}

// transition runs the shared RESERVED→terminal machinery: lock the header
// row FOR UPDATE (serializing against concurrent transitions), let decide
// short-circuit on the current status ("" for a missing row), then apply the
// per-line effect in (warehouse_id, sku_id) lock order and flip the status.
func (r *ReservationRepository) transition(
	ctx context.Context,
	id, targetStatus string,
	decide func(status string) (string, error),
	applyLine func(tx pgx.Tx, a domain.Allocation) error,
) (string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin transition: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status FROM inventory_reservations WHERE id = $1 FOR UPDATE`, id).
		Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("lock reservation %s: %w", id, translateConcurrency(err))
	}
	if done, err := decide(status); done != "" || err != nil {
		return done, err
	}

	// Status is RESERVED: apply the per-line effect. Lines are loaded in
	// (warehouse_id, sku_id) order so balance-row locks are acquired in the
	// same global order as Reserve's FOR UPDATE scan.
	lines, err := loadReservationLines(ctx, tx, id)
	if err != nil {
		return "", err
	}
	for _, a := range lines {
		if err := applyLine(tx, a); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE inventory_reservations SET status = $2, updated_at = now() WHERE id = $1`,
		id, targetStatus); err != nil {
		return "", fmt.Errorf("set reservation %s status: %w", id, translateConcurrency(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit transition %s: %w", id, translateConcurrency(err))
	}
	return targetStatus, nil
}

// GetReservation returns the reservation header and its lines. Lines are
// immutable after Reserve commits, so the two reads need no transaction.
func (r *ReservationRepository) GetReservation(ctx context.Context, id string) (domain.Reservation, error) {
	var zero domain.Reservation
	var res domain.Reservation
	var createdAt, updatedAt time.Time
	var expiresAt *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT external_reference, status, created_at, updated_at, expires_at
		FROM inventory_reservations WHERE id = $1`, id).
		Scan(&res.OrderID, &res.Status, &createdAt, &updatedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, fmt.Errorf("reservation %s: %w", id, domain.ErrReservationNotFound)
	}
	if err != nil {
		return zero, fmt.Errorf("load reservation %s: %w", id, err)
	}
	res.ID = id
	res.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	res.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	if expiresAt != nil {
		res.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	}
	if res.Allocations, err = loadReservationLines(ctx, r.pool, id); err != nil {
		return zero, err
	}
	return res, nil
}

// loadReservationLines returns the reservation's lines as allocations,
// ordered by (warehouse_id, sku_id) — the global balance-lock order.
func loadReservationLines(ctx context.Context, q querier, id string) ([]domain.Allocation, error) {
	rows, err := q.Query(ctx, `
		SELECT sku_id, warehouse_id, quantity
		FROM inventory_reservation_lines
		WHERE reservation_id = $1
		ORDER BY warehouse_id, sku_id`, id)
	if err != nil {
		return nil, fmt.Errorf("load reservation lines %s: %w", id, translateConcurrency(err))
	}
	defer rows.Close()

	var out []domain.Allocation
	for rows.Next() {
		var a domain.Allocation
		if err := rows.Scan(&a.SKUID, &a.WarehouseID, &a.Quantity); err != nil {
			return nil, fmt.Errorf("reservation line scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reservation lines rows: %w", translateConcurrency(err))
	}
	return out, nil
}

// skuIDsOf projects the line SKUs for ANY($1) parameters.
func skuIDsOf(items []domain.Line) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.SKUID)
	}
	return ids
}

// fulfillsLines reports whether one warehouse's ATP satisfies every line.
func fulfillsLines(atp map[string]int64, items []domain.Line) bool {
	for _, it := range items {
		if atp[it.SKUID] < it.Quantity {
			return false
		}
	}
	return true
}

// classifyReservationFailure decides what a failed allocation MEANS before it
// crosses the domain boundary: a shortage is a quantity verdict, and a
// quantity verdict about a SKU with no balance row anywhere would be
// fabricated. Mirrors the availability read's tracked/untracked split
// (TrackedSKUs) inside the reservation transaction, and — like checkout's
// fail-closed precedence — the data gap wins over the shortage in a mixed
// basket. On a classification read error, fail toward the storage error: the
// caller's fail-closed default is retryable, which is the safe direction.
func classifyReservationFailure(ctx context.Context, tx pgx.Tx, items []domain.Line, short []domain.Shortage) error {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.SKUID)
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT sku_id FROM inventory_balances WHERE sku_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("classify reservation failure: %w", err)
	}
	defer rows.Close()
	tracked := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("classify reservation failure scan: %w", err)
		}
		tracked[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("classify reservation failure rows: %w", err)
	}
	var unknown []string
	for _, id := range ids {
		if !tracked[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return &domain.UnknownSKUError{SKUIDs: unknown}
	}
	return &domain.InsufficientStockError{Shortages: short}
}

// shortagesAgainst lists the lines base cannot satisfy; a SKU absent from
// base counts as ATP 0.
func shortagesAgainst(base map[string]int64, items []domain.Line) []domain.Shortage {
	var out []domain.Shortage
	for _, it := range items {
		if atp := base[it.SKUID]; atp < it.Quantity {
			out = append(out, domain.Shortage{SKUID: it.SKUID, Requested: it.Quantity, ATP: atp})
		}
	}
	return out
}
