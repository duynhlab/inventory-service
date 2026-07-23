package v1

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// fakeReservationRepo is a hand-written ReservationStore double. gotReq
// captures what the logic layer actually passed, so aggregation is
// assertable.
type fakeReservationRepo struct {
	result domain.ReservationResult
	status string
	res    domain.Reservation
	err    error

	gotReq domain.ReservationRequest
}

func (f *fakeReservationRepo) Reserve(_ context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	f.gotReq = req
	return f.result, f.err
}

func (f *fakeReservationRepo) Release(_ context.Context, _, _ string) (string, error) {
	return f.status, f.err
}

func (f *fakeReservationRepo) Commit(_ context.Context, _ string) (string, error) {
	return f.status, f.err
}

func (f *fakeReservationRepo) GetReservation(_ context.Context, id string) (domain.Reservation, error) {
	if f.err != nil {
		return domain.Reservation{}, f.err
	}
	return f.res, nil
}

func TestReservationService_Reserve(t *testing.T) {
	t.Run("aggregates duplicate SKU lines before the repository", func(t *testing.T) {
		repo := &fakeReservationRepo{result: domain.ReservationResult{ID: "res-1", Status: domain.ReservationReserved}}
		svc := NewReservationService(repo)
		_, err := svc.Reserve(context.Background(), domain.ReservationRequest{
			ID: "res-1", OrderID: "res-1",
			Items: []domain.Line{
				{SKUID: "sku-a", Quantity: 2},
				{SKUID: "sku-b", Quantity: 1},
				{SKUID: "sku-a", Quantity: 3},
			},
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		want := []domain.Line{{SKUID: "sku-a", Quantity: 5}, {SKUID: "sku-b", Quantity: 1}}
		if !reflect.DeepEqual(repo.gotReq.Items, want) {
			t.Errorf("repo received items %+v, want aggregated %+v", repo.gotReq.Items, want)
		}
		// The repository hashes exactly what it receives, so aggregated items
		// must reproduce the transport's canonical hash.
		if got := domain.CanonicalHash(repo.gotReq.Items, ""); got != domain.CanonicalHash(want, "") {
			t.Errorf("aggregation broke hash canonicalization: %q", got)
		}
	})

	t.Run("passes the result through", func(t *testing.T) {
		want := domain.ReservationResult{
			ID: "res-1", Status: domain.ReservationReserved,
			Allocations: []domain.Allocation{{SKUID: "sku-a", WarehouseID: 1, Quantity: 5}},
		}
		svc := NewReservationService(&fakeReservationRepo{result: want})
		got, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("result = %+v, want %+v", got, want)
		}
	})

	t.Run("errors keep their domain identity through the wrap", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{
			err: &domain.InsufficientStockError{Shortages: []domain.Shortage{{SKUID: "sku-a", Requested: 2, ATP: 0}}},
		})
		_, err := svc.Reserve(context.Background(), domain.ReservationRequest{ID: "res-1"})
		if !errors.Is(err, domain.ErrInsufficientStock) {
			t.Fatalf("err = %v, want ErrInsufficientStock", err)
		}
		var short *domain.InsufficientStockError
		if !errors.As(err, &short) || len(short.Shortages) != 1 {
			t.Fatalf("err = %v, want wrapped InsufficientStockError with detail", err)
		}
	})
}

func TestReservationService_ReleaseCommitGet(t *testing.T) {
	t.Run("release and commit pass status through", func(t *testing.T) {
		svc := NewReservationService(&fakeReservationRepo{status: domain.ReservationReleased})
		if status, err := svc.Release(context.Background(), "res-1", "compensate"); err != nil || status != domain.ReservationReleased {
			t.Errorf("release = (%q, %v), want released", status, err)
		}
		svc = NewReservationService(&fakeReservationRepo{status: domain.ReservationCommitted})
		if status, err := svc.Commit(context.Background(), "res-1"); err != nil || status != domain.ReservationCommitted {
			t.Errorf("commit = (%q, %v), want committed", status, err)
		}
	})

	t.Run("get passes the reservation through and surfaces not-found", func(t *testing.T) {
		want := domain.Reservation{ID: "res-1", OrderID: "order-1", Status: domain.ReservationReserved}
		svc := NewReservationService(&fakeReservationRepo{res: want})
		got, err := svc.GetReservation(context.Background(), "res-1")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("get = (%+v, %v), want %+v", got, err, want)
		}
		svc = NewReservationService(&fakeReservationRepo{err: domain.ErrReservationNotFound})
		if _, err := svc.GetReservation(context.Background(), "ghost"); !errors.Is(err, domain.ErrReservationNotFound) {
			t.Errorf("get missing = %v, want ErrReservationNotFound", err)
		}
	})
}

// TestReservationService_Metrics drives one call per (operation, outcome)
// cell and asserts the counter matches exactly — including that a canceled
// context is never counted.
func TestReservationService_Metrics(t *testing.T) {
	ctx := context.Background()
	before := reservationOutcomeCounts(t) // shared reader is cumulative: assert this test's delta
	reserve := func(repo *fakeReservationRepo) {
		svc := NewReservationService(repo)
		_, _ = svc.Reserve(ctx, domain.ReservationRequest{ID: "res-1"})
	}

	reserve(&fakeReservationRepo{result: domain.ReservationResult{Status: domain.ReservationReserved}})
	reserve(&fakeReservationRepo{result: domain.ReservationResult{Status: domain.ReservationReserved, Replayed: true}})
	reserve(&fakeReservationRepo{err: &domain.InsufficientStockError{}})
	reserve(&fakeReservationRepo{err: domain.ErrIdempotencyConflict})
	reserve(&fakeReservationRepo{err: domain.ErrConcurrencyConflict})
	reserve(&fakeReservationRepo{err: errors.New("db down")})
	reserve(&fakeReservationRepo{err: fmt.Errorf("query: %w", context.Canceled)}) // never counted

	relSvc := NewReservationService(&fakeReservationRepo{status: domain.ReservationReleased})
	_, _ = relSvc.Release(ctx, "res-1", "")
	relSvc = NewReservationService(&fakeReservationRepo{err: domain.ErrInvalidTransition})
	_, _ = relSvc.Release(ctx, "res-1", "")

	cmtSvc := NewReservationService(&fakeReservationRepo{status: domain.ReservationCommitted})
	_, _ = cmtSvc.Commit(ctx, "res-1")
	cmtSvc = NewReservationService(&fakeReservationRepo{err: domain.ErrReservationNotFound})
	_, _ = cmtSvc.Commit(ctx, "res-1")

	want := map[[2]string]int64{
		{"reserve", "ok"}:                 1,
		{"reserve", "replayed"}:           1,
		{"reserve", "insufficient"}:       1,
		{"reserve", "conflict"}:           1, // idempotency divergence
		{"reserve", "concurrency"}:        1, // lock/serialization abort, split from conflict
		{"reserve", "error"}:              1,
		{"release", "ok"}:                 1,
		{"release", "invalid_transition"}: 1,
		{"commit", "ok"}:                  1,
		{"commit", "not_found"}:           1,
	}
	got := reservationOutcomeCounts(t)
	for key, n := range before {
		if got[key] -= n; got[key] == 0 {
			delete(got, key)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reservation.total delta = %v, want %v", got, want)
	}
}

// reservationOutcomeCounts collects the shared test reader and returns
// inventory.reservation.total datapoints keyed by (operation, outcome).
func reservationOutcomeCounts(t *testing.T) map[[2]string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := make(map[[2]string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "inventory.reservation.total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				op, _ := dp.Attributes.Value(attribute.Key("operation"))
				outcome, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[[2]string{op.AsString(), outcome.AsString()}] += dp.Value
			}
		}
	}
	return out
}
