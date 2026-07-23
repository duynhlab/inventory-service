//go:build integration

// Reservation command integration tests (RFC-0021 P1-5, the CP-1 gate). They
// prove — against a real Postgres — that Reserve claims via the reservation
// row, allocates from one warehouse under FOR UPDATE locks, rolls the whole
// transaction back on any shortage, and that the RESERVED→COMMITTED/RELEASED
// FSM applies each effect exactly once. Concurrency tests carry "Concurrency"
// in their names so the stress gate regex 'Concurrency|Race' catches them:
//
//	go test -tags=integration -race -run 'Concurrency|Race' -count=20 ./internal/core/repository/...
package repository

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// reservationFixture wraps the per-test container with the helpers every
// reservation test needs.
type reservationFixture struct {
	pool *pgxpool.Pool
	repo *ReservationRepository
	wh   int64 // default warehouse id
}

func newReservationFixture(t *testing.T) *reservationFixture {
	t.Helper()
	pool := newTestDB(t)
	var wh int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM warehouses WHERE code = 'WH-DEFAULT'`).Scan(&wh); err != nil {
		t.Fatalf("default warehouse: %v", err)
	}
	return &reservationFixture{pool: pool, repo: NewReservationRepository(pool), wh: wh}
}

func (f *reservationFixture) seedBalance(t *testing.T, sku string, wh, onHand, reserved, safety int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
		 VALUES ($1, $2, $3, $4, $5)`, sku, wh, onHand, reserved, safety); err != nil {
		t.Fatalf("seed balance %s: %v", sku, err)
	}
}

func (f *reservationFixture) balance(t *testing.T, sku string, wh int64) (onHand, reserved int64) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT on_hand, reserved FROM inventory_balances WHERE sku_id = $1 AND warehouse_id = $2`,
		sku, wh).Scan(&onHand, &reserved); err != nil {
		t.Fatalf("read balance %s: %v", sku, err)
	}
	return onHand, reserved
}

func (f *reservationFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func lines(pairs ...domain.Line) []domain.Line { return pairs }

func req(id string, items ...domain.Line) domain.ReservationRequest {
	return domain.ReservationRequest{ID: id, OrderID: id, Items: items}
}

// checkLedgerInvariant proves the balance equals the replay of the movement
// ledger for a SKU seeded with (seededOnHand, seededReserved=0): after any
// mix of reserve/release/commit, on_hand == seeded + SUM(on_hand_delta) and
// reserved == SUM(reserved_delta).
func (f *reservationFixture) checkLedgerInvariant(t *testing.T, sku string, wh, seededOnHand int64) {
	t.Helper()
	var onHandSum, reservedSum int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(on_hand_delta), 0), COALESCE(SUM(reserved_delta), 0)
		 FROM inventory_movements WHERE sku_id = $1 AND warehouse_id = $2`,
		sku, wh).Scan(&onHandSum, &reservedSum); err != nil {
		t.Fatalf("ledger sums for %s: %v", sku, err)
	}
	onHand, reserved := f.balance(t, sku, wh)
	if onHand != seededOnHand+onHandSum {
		t.Errorf("%s: on_hand = %d, want seeded %d + ledger %d", sku, onHand, seededOnHand, onHandSum)
	}
	if reserved != reservedSum {
		t.Errorf("%s: reserved = %d, want ledger sum %d", sku, reserved, reservedSum)
	}
}

func TestReservationConcurrency(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()

	t.Run("two racing reserves for the last unit -> exactly one wins", func(t *testing.T) {
		f.seedBalance(t, "race-last", f.wh, 1, 0, 0)

		type outcome struct {
			res domain.ReservationResult
			err error
		}
		results := make([]outcome, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, id := range []string{"race-a", "race-b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := f.repo.Reserve(ctx, req(id, domain.Line{SKUID: "race-last", Quantity: 1}))
				results[i] = outcome{r, err}
			}()
		}
		close(start)
		wg.Wait()

		var wins, shorts int
		for _, o := range results {
			switch {
			case o.err == nil && o.res.Status == domain.ReservationReserved:
				wins++
			case errors.Is(o.err, domain.ErrInsufficientStock):
				shorts++
			default:
				t.Errorf("unexpected outcome (%+v, %v)", o.res, o.err)
			}
		}
		if wins != 1 || shorts != 1 {
			t.Fatalf("wins=%d shorts=%d, want exactly 1 RESERVED and 1 ErrInsufficientStock", wins, shorts)
		}
		onHand, reserved := f.balance(t, "race-last", f.wh)
		if reserved > onHand || reserved != 1 {
			t.Errorf("balance on_hand=%d reserved=%d, want reserved=1 and reserved <= on_hand", onHand, reserved)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM inventory_movements WHERE sku_id = 'race-last'`); n != 1 {
			t.Errorf("movement rows = %d, want exactly 1", n)
		}
	})

	t.Run("two racing identical reserves -> one claims, one replays same allocations", func(t *testing.T) {
		f.seedBalance(t, "race-replay", f.wh, 5, 0, 0)
		request := req("race-same", domain.Line{SKUID: "race-replay", Quantity: 2})

		type outcome struct {
			res domain.ReservationResult
			err error
		}
		results := make([]outcome, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := f.repo.Reserve(ctx, request)
				results[i] = outcome{r, err}
			}()
		}
		close(start)
		wg.Wait()

		var replays int
		wantAlloc := []domain.Allocation{{SKUID: "race-replay", WarehouseID: f.wh, Quantity: 2}}
		for _, o := range results {
			if o.err != nil {
				t.Fatalf("identical reserve failed: %v", o.err)
			}
			if o.res.Status != domain.ReservationReserved {
				t.Errorf("status = %q, want reserved", o.res.Status)
			}
			if len(o.res.Allocations) != 1 || o.res.Allocations[0] != wantAlloc[0] {
				t.Errorf("allocations = %+v, want %+v", o.res.Allocations, wantAlloc)
			}
			if o.res.Replayed {
				replays++
			}
		}
		if replays != 1 {
			t.Errorf("replayed results = %d, want exactly 1", replays)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM inventory_reservation_lines WHERE reservation_id = 'race-same'`); n != 1 {
			t.Errorf("line rows = %d, want exactly 1", n)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM inventory_movements WHERE sku_id = 'race-replay'`); n != 1 {
			t.Errorf("movement rows = %d, want exactly 1 (replay must not double-apply)", n)
		}
		if _, reserved := f.balance(t, "race-replay", f.wh); reserved != 2 {
			t.Errorf("reserved = %d, want 2 (applied exactly once)", reserved)
		}
	})
}

func TestReserveAllocationAndRollback(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()

	// A second active warehouse with more stock than the default: allocation
	// must still prefer the lowest-id warehouse that fulfills the WHOLE order.
	var wh2 int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO warehouses (code, name, status) VALUES ('WH-2', 'Second', 'active') RETURNING id`).
		Scan(&wh2); err != nil {
		t.Fatalf("insert second warehouse: %v", err)
	}

	t.Run("one short line rolls back the whole reservation", func(t *testing.T) {
		f.seedBalance(t, "roll-a", f.wh, 10, 0, 0)
		f.seedBalance(t, "roll-b", f.wh, 1, 0, 0)

		_, err := f.repo.Reserve(ctx, req("roll-1",
			domain.Line{SKUID: "roll-a", Quantity: 2},
			domain.Line{SKUID: "roll-b", Quantity: 5}))
		if !errors.Is(err, domain.ErrInsufficientStock) {
			t.Fatalf("err = %v, want ErrInsufficientStock", err)
		}
		var short *domain.InsufficientStockError
		if !errors.As(err, &short) {
			t.Fatalf("err %v does not carry InsufficientStockError detail", err)
		}
		want := []domain.Shortage{{SKUID: "roll-b", Requested: 5, ATP: 1}}
		if len(short.Shortages) != 1 || short.Shortages[0] != want[0] {
			t.Errorf("shortages = %+v, want %+v", short.Shortages, want)
		}

		if n := f.count(t, `SELECT COUNT(*) FROM inventory_reservations WHERE id = 'roll-1'`); n != 0 {
			t.Errorf("claim rows = %d, want 0 (whole tx must roll back)", n)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM inventory_movements WHERE sku_id IN ('roll-a', 'roll-b')`); n != 0 {
			t.Errorf("movement rows = %d, want 0", n)
		}
		for _, sku := range []string{"roll-a", "roll-b"} {
			if _, reserved := f.balance(t, sku, f.wh); reserved != 0 {
				t.Errorf("%s reserved = %d, want 0", sku, reserved)
			}
		}
	})

	t.Run("allocates from the lowest-id warehouse fulfilling every line", func(t *testing.T) {
		// Default warehouse cannot fulfill alloc-b; wh2 fulfills both. A SKU
		// with no balance row in the default warehouse counts as ATP 0 there.
		f.seedBalance(t, "alloc-a", f.wh, 10, 0, 0)
		f.seedBalance(t, "alloc-a", wh2, 10, 0, 0)
		f.seedBalance(t, "alloc-b", wh2, 10, 0, 0)

		res, err := f.repo.Reserve(ctx, req("alloc-1",
			domain.Line{SKUID: "alloc-a", Quantity: 1},
			domain.Line{SKUID: "alloc-b", Quantity: 1}))
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		for _, a := range res.Allocations {
			if a.WarehouseID != wh2 {
				t.Errorf("allocation %+v, want warehouse %d", a, wh2)
			}
		}
	})

	t.Run("no warehouse fulfills -> shortages against lowest-id active warehouse", func(t *testing.T) {
		f.seedBalance(t, "short-a", wh2, 10, 0, 0) // nothing in default warehouse

		_, err := f.repo.Reserve(ctx, req("short-1", domain.Line{SKUID: "short-a", Quantity: 20}))
		var short *domain.InsufficientStockError
		if !errors.As(err, &short) {
			t.Fatalf("err = %v, want InsufficientStockError", err)
		}
		// Baseline is the default (lowest-id active) warehouse where the SKU
		// has no balance row at all: ATP reports 0, same rule as
		// CheckAvailability.
		want := domain.Shortage{SKUID: "short-a", Requested: 20, ATP: 0}
		if len(short.Shortages) != 1 || short.Shortages[0] != want {
			t.Errorf("shortages = %+v, want [%+v]", short.Shortages, want)
		}
	})
}

func TestReservationIdempotency(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()
	f.seedBalance(t, "idem-a", f.wh, 10, 0, 0)

	r1 := req("idem-1", domain.Line{SKUID: "idem-a", Quantity: 3})
	r1.ExpiresAt = "2026-12-31T00:00:00Z"
	first, err := f.repo.Reserve(ctx, r1)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if first.Replayed {
		t.Error("first reserve marked replayed")
	}

	t.Run("expires_at round-trips through GetReservation", func(t *testing.T) {
		got, err := f.repo.GetReservation(ctx, "idem-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ExpiresAt == "" {
			t.Errorf("expires_at empty, want the recorded expiry echoed back")
		}
	})

	t.Run("sequential replay returns the original result", func(t *testing.T) {
		replay, err := f.repo.Reserve(ctx, req("idem-1", domain.Line{SKUID: "idem-a", Quantity: 3}))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if !replay.Replayed || replay.Status != domain.ReservationReserved {
			t.Errorf("replay = %+v, want replayed RESERVED", replay)
		}
		if len(replay.Allocations) != 1 || replay.Allocations[0] != first.Allocations[0] {
			t.Errorf("replay allocations = %+v, want %+v", replay.Allocations, first.Allocations)
		}
		if _, reserved := f.balance(t, "idem-a", f.wh); reserved != 3 {
			t.Errorf("reserved = %d, want 3 (replay must not re-apply)", reserved)
		}
	})

	t.Run("same reservation id with a divergent hash conflicts", func(t *testing.T) {
		_, err := f.repo.Reserve(ctx, req("idem-1", domain.Line{SKUID: "idem-a", Quantity: 4}))
		if !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("divergent replay err = %v, want ErrIdempotencyConflict", err)
		}
	})

	t.Run("same id and items with a different order id conflicts", func(t *testing.T) {
		// The hash covers items+destination only, so a divergent order
		// reference must be caught by the explicit external_reference
		// compare in the claimed=false replay path.
		r := req("idem-1", domain.Line{SKUID: "idem-a", Quantity: 3})
		r.OrderID = "idem-other-order"
		_, err := f.repo.Reserve(ctx, r)
		if !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("order mismatch replay err = %v, want ErrIdempotencyConflict", err)
		}
		if _, reserved := f.balance(t, "idem-a", f.wh); reserved != 3 {
			t.Errorf("reserved = %d, want 3 (conflict must not change state)", reserved)
		}
	})

	t.Run("different reservation id reusing the order id conflicts", func(t *testing.T) {
		r := req("idem-2", domain.Line{SKUID: "idem-a", Quantity: 1})
		r.OrderID = "idem-1" // already claimed by reservation idem-1
		_, err := f.repo.Reserve(ctx, r)
		if !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("order reuse err = %v, want ErrIdempotencyConflict", err)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM inventory_reservations WHERE id = 'idem-2'`); n != 0 {
			t.Errorf("conflicting reservation persisted (%d rows), want 0", n)
		}
	})
}

func TestReservationFSM(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()
	f.seedBalance(t, "fsm-a", f.wh, 10, 0, 0)
	f.seedBalance(t, "fsm-b", f.wh, 10, 0, 0)

	t.Run("commit decrements exactly once, replay returns COMMITTED", func(t *testing.T) {
		if _, err := f.repo.Reserve(ctx, req("fsm-commit",
			domain.Line{SKUID: "fsm-a", Quantity: 4})); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		status, err := f.repo.Commit(ctx, "fsm-commit")
		if err != nil || status != domain.ReservationCommitted {
			t.Fatalf("commit = (%q, %v), want committed", status, err)
		}
		status, err = f.repo.Commit(ctx, "fsm-commit") // replay
		if err != nil || status != domain.ReservationCommitted {
			t.Fatalf("commit replay = (%q, %v), want committed no-op", status, err)
		}
		onHand, reserved := f.balance(t, "fsm-a", f.wh)
		if onHand != 6 || reserved != 0 {
			t.Errorf("balance = (on_hand=%d, reserved=%d), want (6, 0): once-effect", onHand, reserved)
		}
		if n := f.count(t,
			`SELECT COUNT(*) FROM inventory_movements WHERE sku_id = 'fsm-a' AND type = 'SALE_COMMITTED'`); n != 1 {
			t.Errorf("SALE_COMMITTED rows = %d, want 1", n)
		}
	})

	t.Run("release after commit -> ErrInvalidTransition", func(t *testing.T) {
		if _, err := f.repo.Release(ctx, "fsm-commit", "compensate"); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("release committed = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("release returns stock; commit after release -> ErrInvalidTransition", func(t *testing.T) {
		if _, err := f.repo.Reserve(ctx, req("fsm-release",
			domain.Line{SKUID: "fsm-b", Quantity: 3})); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		status, err := f.repo.Release(ctx, "fsm-release", "saga_compensation")
		if err != nil || status != domain.ReservationReleased {
			t.Fatalf("release = (%q, %v), want released", status, err)
		}
		if onHand, reserved := f.balance(t, "fsm-b", f.wh); onHand != 10 || reserved != 0 {
			t.Errorf("balance = (%d, %d), want (10, 0) after release", onHand, reserved)
		}
		// Idempotent re-release.
		if status, err := f.repo.Release(ctx, "fsm-release", "again"); err != nil || status != domain.ReservationReleased {
			t.Fatalf("re-release = (%q, %v), want released no-op", status, err)
		}
		if _, err := f.repo.Commit(ctx, "fsm-release"); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("commit released = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("release of a missing reservation is a no-op success", func(t *testing.T) {
		status, err := f.repo.Release(ctx, "fsm-ghost", "compensate")
		if err != nil || status != domain.ReservationReleased {
			t.Fatalf("release missing = (%q, %v), want released no-op", status, err)
		}
	})

	t.Run("commit of a missing reservation -> ErrReservationNotFound", func(t *testing.T) {
		if _, err := f.repo.Commit(ctx, "fsm-ghost"); !errors.Is(err, domain.ErrReservationNotFound) {
			t.Fatalf("commit missing = %v, want ErrReservationNotFound", err)
		}
	})

	t.Run("GetReservation returns header and lines", func(t *testing.T) {
		got, err := f.repo.GetReservation(ctx, "fsm-commit")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != "fsm-commit" || got.OrderID != "fsm-commit" || got.Status != domain.ReservationCommitted {
			t.Errorf("reservation = %+v, want committed fsm-commit", got)
		}
		wantAlloc := domain.Allocation{SKUID: "fsm-a", WarehouseID: f.wh, Quantity: 4}
		if len(got.Allocations) != 1 || got.Allocations[0] != wantAlloc {
			t.Errorf("allocations = %+v, want [%+v]", got.Allocations, wantAlloc)
		}
		if got.CreatedAt == "" || got.UpdatedAt == "" {
			t.Errorf("timestamps missing: %+v", got)
		}
		if _, err := f.repo.GetReservation(ctx, "fsm-ghost"); !errors.Is(err, domain.ErrReservationNotFound) {
			t.Errorf("get missing = %v, want ErrReservationNotFound", err)
		}
	})

	t.Run("movements audit reservation lifecycle without actor", func(t *testing.T) {
		var n int64
		if err := f.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM inventory_movements
			 WHERE reference_type = 'reservation' AND (actor IS NOT NULL OR reference_id NOT IN ('fsm-commit', 'fsm-release'))`).
			Scan(&n); err != nil {
			t.Fatalf("audit movements: %v", err)
		}
		if n != 0 {
			t.Errorf("%d reservation movements with actor set or wrong reference_id, want 0", n)
		}
	})

	t.Run("ledger invariant: balances equal seeded stock plus movement deltas", func(t *testing.T) {
		f.checkLedgerInvariant(t, "fsm-a", f.wh, 10)
		f.checkLedgerInvariant(t, "fsm-b", f.wh, 10)
	})
}

// TestReservationCommandIDEnvelope proves the transport's 186-char id cap is
// exactly the envelope the ledger schema allows: a maximum-length id with a
// maximum-length sku reserves and commits end-to-end (command_id
// res:/cmt:<id>:<sku> = 255 chars, VARCHAR(255) boundary).
func TestReservationCommandIDEnvelope(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()

	maxSKU := strings.Repeat("s", 64)
	maxID := strings.Repeat("r", 186)
	f.seedBalance(t, maxSKU, f.wh, 5, 0, 0)

	res, err := f.repo.Reserve(ctx, req(maxID, domain.Line{SKUID: maxSKU, Quantity: 2}))
	if err != nil {
		t.Fatalf("reserve at envelope boundary: %v", err)
	}
	if res.Status != domain.ReservationReserved {
		t.Fatalf("status = %q, want reserved", res.Status)
	}
	if status, err := f.repo.Commit(ctx, maxID); err != nil || status != domain.ReservationCommitted {
		t.Fatalf("commit at envelope boundary = (%q, %v), want committed", status, err)
	}
	var cmdLen int64
	if err := f.pool.QueryRow(ctx,
		`SELECT MAX(LENGTH(command_id)) FROM inventory_movements WHERE sku_id = $1`, maxSKU).
		Scan(&cmdLen); err != nil {
		t.Fatalf("command_id length: %v", err)
	}
	if cmdLen != 255 {
		t.Errorf("max command_id length = %d, want exactly 255 (boundary proof)", cmdLen)
	}
}

// TestTransitionFailsClosedOnMissingBalance proves the RowsAffected guard: a
// balance row that vanished between Reserve and Commit must abort the whole
// transition — the FSM may never flip without moving stock.
func TestTransitionFailsClosedOnMissingBalance(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()
	f.seedBalance(t, "gone-a", f.wh, 5, 0, 0)

	if _, err := f.repo.Reserve(ctx, req("gone-1", domain.Line{SKUID: "gone-a", Quantity: 2})); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Simulate operator damage: the balance row disappears out-of-band.
	// (Postgres allows it — reservation lines don't FK inventory_balances.)
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM inventory_balances WHERE sku_id = 'gone-a' AND warehouse_id = $1`, f.wh); err != nil {
		t.Fatalf("delete balance: %v", err)
	}

	if _, err := f.repo.Commit(ctx, "gone-1"); err == nil {
		t.Fatal("commit with vanished balance row succeeded, want fail-closed error")
	}
	got, err := f.repo.GetReservation(ctx, "gone-1")
	if err != nil {
		t.Fatalf("get after failed commit: %v", err)
	}
	if got.Status != domain.ReservationReserved {
		t.Errorf("status = %q, want reserved (failed transition must not flip the FSM)", got.Status)
	}
	if _, err := f.repo.Release(ctx, "gone-1", "cleanup"); err == nil {
		t.Fatal("release with vanished balance row succeeded, want fail-closed error")
	}
}
