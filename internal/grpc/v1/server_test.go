package v1

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	logicv1 "github.com/duynhlab/inventory-service/internal/logic/v1"
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// availabilityStub is a configurable Availability double. gotItems captures
// what the transport actually passed, so aggregation is assertable.
type availabilityStub struct {
	availabilities []logicv1.SkuAvailability
	check          *logicv1.CheckResult
	err            error

	gotItems []logicv1.CheckItem
}

func (s *availabilityStub) BatchGetAvailability(_ context.Context, _ []string) ([]logicv1.SkuAvailability, error) {
	return s.availabilities, s.err
}

func (s *availabilityStub) CheckAvailability(_ context.Context, items []logicv1.CheckItem) (*logicv1.CheckResult, error) {
	s.gotItems = items
	return s.check, s.err
}

func newTestServer(stub *availabilityStub) *Server {
	return NewServer(stub, nil, zap.NewNop())
}

// wantValidationError asserts the grpcx VALIDATION_ERROR contract: callers
// (checkout, the saga) switch on the reason, not the message.
func wantValidationError(t *testing.T, err error) {
	t.Helper()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if got := grpcx.Reason(err); got != grpcx.ReasonValidationError {
		t.Fatalf("reason = %q, want %q", got, grpcx.ReasonValidationError)
	}
}

func TestServer_BatchGetAvailability(t *testing.T) {
	t.Run("success maps logic results to proto", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{availabilities: []logicv1.SkuAvailability{
			{SKUID: "sku-in", Status: logicv1.StatusInStock, ATP: 7},
			{SKUID: "sku-low", Status: logicv1.StatusLowStock, ATP: 2},
			{SKUID: "sku-out", Status: logicv1.StatusOutOfStock, ATP: 0},
			{SKUID: "sku-unknown", Status: logicv1.StatusUnknown, ATP: 0},
		}})
		resp, err := srv.BatchGetAvailability(context.Background(),
			&inventoryv1.BatchGetAvailabilityRequest{
				SkuIds: []string{"sku-in", "sku-low", "sku-out", "sku-unknown"},
			})
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		got := resp.GetAvailabilities()
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4", len(got))
		}
		wantStatus := []inventoryv1.AvailabilityStatus{
			inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_IN_STOCK,
			inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_LOW_STOCK,
			inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_OUT_OF_STOCK,
			inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_UNKNOWN,
		}
		for i, w := range wantStatus {
			if got[i].GetStatus() != w {
				t.Errorf("availabilities[%d].status = %v, want %v", i, got[i].GetStatus(), w)
			}
		}
		if got[0].GetSkuId() != "sku-in" || got[0].GetAvailableToPromise() != 7 {
			t.Errorf("availabilities[0] = %+v, want sku-in atp=7", got[0])
		}
	})

	t.Run("empty sku_ids -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.BatchGetAvailability(context.Background(),
			&inventoryv1.BatchGetAvailabilityRequest{})
		wantValidationError(t, err)
	})

	t.Run("oversized batch -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		ids := make([]string, maxBatchSkuIDs+1)
		for i := range ids {
			ids[i] = "sku-a"
		}
		_, err := srv.BatchGetAvailability(context.Background(),
			&inventoryv1.BatchGetAvailabilityRequest{SkuIds: ids})
		wantValidationError(t, err)
	})

	t.Run("hostile sku charset -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.BatchGetAvailability(context.Background(),
			&inventoryv1.BatchGetAvailabilityRequest{SkuIds: []string{"sku'; DROP TABLE --"}})
		wantValidationError(t, err)
	})

	t.Run("empty-string sku -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.BatchGetAvailability(context.Background(),
			&inventoryv1.BatchGetAvailabilityRequest{SkuIds: []string{""}})
		wantValidationError(t, err)
	})

	t.Run("logic error -> Unavailable with DEPENDENCY_UNAVAILABLE", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{err: errors.New("db down")})
		_, err := srv.BatchGetAvailability(context.Background(),
			&inventoryv1.BatchGetAvailabilityRequest{SkuIds: []string{"sku-a"}})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable", status.Code(err))
		}
		if got := grpcx.Reason(err); got != grpcx.ReasonDependencyUnavailable {
			t.Fatalf("reason = %q, want %q", got, grpcx.ReasonDependencyUnavailable)
		}
	})
}

func TestServer_CheckAvailability(t *testing.T) {
	oneItem := []*inventoryv1.ReservationItem{{SkuId: "sku-a", Quantity: 1}}

	t.Run("fulfillable basket", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{check: &logicv1.CheckResult{CanFulfill: true}})
		resp, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{Items: oneItem})
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if !resp.GetCanFulfill() || len(resp.GetShortages()) != 0 {
			t.Errorf("response = %+v, want can_fulfill with no shortages", resp)
		}
	})

	t.Run("shortages map to proto rows", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{check: &logicv1.CheckResult{
			Shortages: []logicv1.ShortageLine{{SKUID: "sku-a", Requested: 5, ATP: 2}},
		}})
		resp, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{
				Items: []*inventoryv1.ReservationItem{{SkuId: "sku-a", Quantity: 5}},
			})
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		if resp.GetCanFulfill() || len(resp.GetShortages()) != 1 {
			t.Fatalf("response = %+v, want one shortage", resp)
		}
		sh := resp.GetShortages()[0]
		if sh.GetSkuId() != "sku-a" || sh.GetRequested() != 5 || sh.GetAvailableToPromise() != 2 {
			t.Errorf("shortage = %+v, want sku-a requested=5 atp=2", sh)
		}
	})

	t.Run("duplicate SKU lines aggregate before the logic layer", func(t *testing.T) {
		// Two lines of 3 against an ATP of 5 must reach logic as one demand
		// of 6 — otherwise each line passes alone and the basket oversells.
		stub := &availabilityStub{check: &logicv1.CheckResult{
			Shortages: []logicv1.ShortageLine{{SKUID: "sku-a", Requested: 6, ATP: 5}},
		}}
		srv := newTestServer(stub)
		resp, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{
				Items: []*inventoryv1.ReservationItem{
					{SkuId: "sku-a", Quantity: 3},
					{SkuId: "sku-a", Quantity: 3},
				},
			})
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		wantItems := []logicv1.CheckItem{{SKUID: "sku-a", Quantity: 6}}
		if !reflect.DeepEqual(stub.gotItems, wantItems) {
			t.Errorf("logic received %+v, want %+v", stub.gotItems, wantItems)
		}
		if resp.GetCanFulfill() || len(resp.GetShortages()) != 1 ||
			resp.GetShortages()[0].GetRequested() != 6 {
			t.Errorf("response = %+v, want one shortage with requested=6", resp)
		}
	})

	t.Run("empty items -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{})
		wantValidationError(t, err)
	})

	t.Run("oversized basket -> VALIDATION_ERROR", func(t *testing.T) {
		items := make([]*inventoryv1.ReservationItem, maxCheckItems+1)
		for i := range items {
			items[i] = &inventoryv1.ReservationItem{SkuId: "sku-a", Quantity: 1}
		}
		srv := newTestServer(&availabilityStub{})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{Items: items})
		wantValidationError(t, err)
	})

	t.Run("non-positive quantity -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{
				Items: []*inventoryv1.ReservationItem{{SkuId: "sku-a", Quantity: 0}},
			})
		wantValidationError(t, err)
	})

	t.Run("quantity above cap -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{
				Items: []*inventoryv1.ReservationItem{{SkuId: "sku-a", Quantity: maxQuantity + 1}},
			})
		wantValidationError(t, err)
	})

	t.Run("hostile sku charset -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{
				Items: []*inventoryv1.ReservationItem{{SkuId: "sku a\n", Quantity: 1}},
			})
		wantValidationError(t, err)
	})

	t.Run("empty-string sku -> VALIDATION_ERROR", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{
				Items: []*inventoryv1.ReservationItem{{SkuId: "", Quantity: 1}},
			})
		wantValidationError(t, err)
	})

	t.Run("logic error -> Unavailable with DEPENDENCY_UNAVAILABLE", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{err: errors.New("db down")})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{Items: oneItem})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable", status.Code(err))
		}
		if got := grpcx.Reason(err); got != grpcx.ReasonDependencyUnavailable {
			t.Fatalf("reason = %q, want %q", got, grpcx.ReasonDependencyUnavailable)
		}
	})

	t.Run("canceled context -> Canceled without reason detail", func(t *testing.T) {
		srv := newTestServer(&availabilityStub{err: context.Canceled})
		_, err := srv.CheckAvailability(context.Background(),
			&inventoryv1.CheckAvailabilityRequest{Items: oneItem})
		if status.Code(err) != codes.Canceled {
			t.Fatalf("code = %v, want Canceled", status.Code(err))
		}
		if got := grpcx.Reason(err); got != "" {
			t.Fatalf("reason = %q, want none", got)
		}
	})
}
