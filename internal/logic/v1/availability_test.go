package v1

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeAvailabilityRepo is a hand-written AvailabilityReader double.
type fakeAvailabilityRepo struct {
	atp       map[string]int64
	tracked   map[string]bool
	balances  map[int64]map[string]int64
	activeIDs []int64
	err       error
	// trackedErr fails only TrackedSKUs, so the second query's error branch
	// is reachable behind a healthy BatchATP.
	trackedErr error
}

func (f *fakeAvailabilityRepo) BatchATP(_ context.Context, _ []string) (map[string]int64, error) {
	return f.atp, f.err
}

func (f *fakeAvailabilityRepo) ActiveWarehouseBalances(_ context.Context, _ []string) (map[int64]map[string]int64, []int64, error) {
	return f.balances, f.activeIDs, f.err
}

func (f *fakeAvailabilityRepo) TrackedSKUs(_ context.Context, _ []string) (map[string]bool, error) {
	if f.trackedErr != nil {
		return nil, f.trackedErr
	}
	return f.tracked, f.err
}

func TestAvailabilityService_BatchGetAvailability(t *testing.T) {
	t.Run("status thresholds, tracked OUT_OF_STOCK, untracked UNKNOWN", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{
			atp: map[string]int64{
				"sku-in":       5, // threshold boundary: 5 is IN_STOCK
				"sku-low":      4, // 1..4 is LOW_STOCK
				"sku-low-edge": 1,
				"sku-out":      0,
			},
			// Tracked but absent from the active-ATP map: its only stock sits
			// in an inactive warehouse — un-promisable, not unknowable.
			tracked: map[string]bool{"sku-inactive-only": true},
		})

		got, err := svc.BatchGetAvailability(context.Background(),
			[]string{"sku-in", "sku-low", "sku-low-edge", "sku-out", "sku-inactive-only", "sku-missing"})
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		want := []SkuAvailability{
			{SKUID: "sku-in", Status: StatusInStock, ATP: 5},
			{SKUID: "sku-low", Status: StatusLowStock, ATP: 4},
			{SKUID: "sku-low-edge", Status: StatusLowStock, ATP: 1},
			{SKUID: "sku-out", Status: StatusOutOfStock, ATP: 0},
			{SKUID: "sku-inactive-only", Status: StatusOutOfStock, ATP: 0},
			{SKUID: "sku-missing", Status: StatusUnknown, ATP: 0},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("availabilities = %+v, want %+v", got, want)
		}
	})

	t.Run("repository error surfaces", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{err: errors.New("db down")})
		if _, err := svc.BatchGetAvailability(context.Background(), []string{"sku-a"}); err == nil {
			t.Fatal("got nil error, want repository error")
		}
	})

	t.Run("tracked-lookup error surfaces", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{trackedErr: errors.New("db down")})
		if _, err := svc.BatchGetAvailability(context.Background(), []string{"sku-missing"}); err == nil {
			t.Fatal("got nil error, want tracked-lookup error")
		}
	})
}

func TestAvailabilityService_CheckAvailability(t *testing.T) {
	items := func(pairs ...CheckItem) []CheckItem { return pairs }

	t.Run("one warehouse satisfies every line (quantity == ATP edge)", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{
			balances:  map[int64]map[string]int64{1: {"sku-a": 3, "sku-b": 1}},
			activeIDs: []int64{1},
		})
		res, err := svc.CheckAvailability(context.Background(),
			items(CheckItem{"sku-a", 3}, CheckItem{"sku-b", 1}))
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if !res.CanFulfill || len(res.Shortages) != 0 {
			t.Errorf("result = %+v, want fulfillable with no shortages", res)
		}
	})

	t.Run("wh1 partial but wh2 full -> fulfillable via wh2", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{
			balances: map[int64]map[string]int64{
				1: {"sku-a": 5, "sku-b": 0},
				2: {"sku-a": 5, "sku-b": 2},
			},
			activeIDs: []int64{1, 2},
		})
		res, err := svc.CheckAvailability(context.Background(),
			items(CheckItem{"sku-a", 4}, CheckItem{"sku-b", 2}))
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if !res.CanFulfill {
			t.Errorf("result = %+v, want fulfillable", res)
		}
	})

	t.Run("no warehouse fulfills -> shortages against lowest-id warehouse", func(t *testing.T) {
		// wh2 has more of sku-a than wh1: shortages must still be computed
		// against wh1 (the deterministic lowest-id policy), so ATP reports 1.
		svc := NewAvailabilityService(&fakeAvailabilityRepo{
			balances: map[int64]map[string]int64{
				2: {"sku-a": 2, "sku-b": 9},
				1: {"sku-a": 1, "sku-b": 9},
			},
			activeIDs: []int64{1, 2},
		})
		res, err := svc.CheckAvailability(context.Background(),
			items(CheckItem{"sku-a", 5}, CheckItem{"sku-b", 9}, CheckItem{"sku-c", 1}))
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if res.CanFulfill {
			t.Fatalf("result = %+v, want shortage", res)
		}
		want := []ShortageLine{
			{SKUID: "sku-a", Requested: 5, ATP: 1},
			{SKUID: "sku-c", Requested: 1, ATP: 0}, // no balance row anywhere
		}
		if !reflect.DeepEqual(res.Shortages, want) {
			t.Errorf("shortages = %+v, want %+v", res.Shortages, want)
		}
	})

	t.Run("lowest active warehouse holds no rows -> shortage base is still it", func(t *testing.T) {
		// wh1 is active but empty for these SKUs; wh2 holds stock yet not
		// enough. Shortages report against wh1 (ATP 0), never against the
		// lowest warehouse that happens to have balance rows.
		svc := NewAvailabilityService(&fakeAvailabilityRepo{
			balances:  map[int64]map[string]int64{2: {"sku-a": 3}},
			activeIDs: []int64{1, 2},
		})
		res, err := svc.CheckAvailability(context.Background(), items(CheckItem{"sku-a", 5}))
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		want := []ShortageLine{{SKUID: "sku-a", Requested: 5, ATP: 0}}
		if res.CanFulfill || !reflect.DeepEqual(res.Shortages, want) {
			t.Errorf("result = %+v, want shortages %+v", res, want)
		}
	})

	t.Run("no active warehouse at all -> every line short at ATP 0", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{})
		res, err := svc.CheckAvailability(context.Background(), items(CheckItem{"sku-a", 2}))
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		want := []ShortageLine{{SKUID: "sku-a", Requested: 2, ATP: 0}}
		if res.CanFulfill || !reflect.DeepEqual(res.Shortages, want) {
			t.Errorf("result = %+v, want shortages %+v", res, want)
		}
	})

	t.Run("repository error surfaces", func(t *testing.T) {
		svc := NewAvailabilityService(&fakeAvailabilityRepo{err: errors.New("db down")})
		if _, err := svc.CheckAvailability(context.Background(), items(CheckItem{"sku-a", 1})); err == nil {
			t.Fatal("got nil error, want repository error")
		}
	})

	t.Run("canceled context is not counted as an error outcome", func(t *testing.T) {
		// The shared reader (TestMain) accumulates across the whole package
		// run, so assert the DELTA this subtest produces: one counted error
		// (db down) and zero for the canceled call.
		ctx := context.Background()
		collect := func() int64 {
			var rm metricdata.ResourceMetrics
			if err := testMetricReader.Collect(ctx, &rm); err != nil {
				t.Fatalf("collect metrics: %v", err)
			}
			return errorOutcomeCount(rm)
		}
		before := collect()

		canceled := NewAvailabilityService(&fakeAvailabilityRepo{
			err: fmt.Errorf("query: %w", context.Canceled),
		})
		if _, err := canceled.CheckAvailability(ctx, items(CheckItem{"sku-a", 1})); err == nil {
			t.Fatal("got nil error, want canceled error")
		}
		down := NewAvailabilityService(&fakeAvailabilityRepo{err: errors.New("db down")})
		if _, err := down.CheckAvailability(ctx, items(CheckItem{"sku-a", 1})); err == nil {
			t.Fatal("got nil error, want repository error")
		}

		if got := collect() - before; got != 1 {
			t.Errorf("outcome=error delta = %d, want 1 (canceled call must not count)", got)
		}
	})
}

// errorOutcomeCount sums inventory.check.total datapoints with outcome=error.
func errorOutcomeCount(rm metricdata.ResourceMetrics) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "inventory.check.total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("outcome")); ok && v.AsString() == "error" {
					total += dp.Value
				}
			}
		}
	}
	return total
}
