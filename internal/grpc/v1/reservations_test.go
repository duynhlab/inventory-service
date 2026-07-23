package v1

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/inventory-service/internal/core/domain"
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// reservationsStub is a configurable Reservations double. gotReq captures
// what the transport actually passed.
type reservationsStub struct {
	result domain.ReservationResult
	status string
	res    domain.Reservation
	err    error

	gotReq    domain.ReservationRequest
	gotReason string
}

func (s *reservationsStub) Reserve(_ context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	s.gotReq = req
	return s.result, s.err
}

func (s *reservationsStub) Release(_ context.Context, _, reason string) (string, error) {
	s.gotReason = reason
	return s.status, s.err
}

func (s *reservationsStub) Commit(_ context.Context, _ string) (string, error) {
	return s.status, s.err
}

func (s *reservationsStub) GetReservation(_ context.Context, _ string) (domain.Reservation, error) {
	if s.err != nil {
		return domain.Reservation{}, s.err
	}
	return s.res, nil
}

func newReservationServer(stub *reservationsStub) *Server {
	return NewServer(nil, stub, zap.NewNop())
}

func reserveRequest() *inventoryv1.ReserveRequest {
	return &inventoryv1.ReserveRequest{
		ReservationId: "order-1",
		OrderId:       "order-1",
		Items:         []*inventoryv1.ReservationItem{{SkuId: "sku-a", Quantity: 2}},
	}
}

func wantReason(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("code = %v, want %v", status.Code(err), code)
	}
	if got := grpcx.Reason(err); got != reason {
		t.Fatalf("reason = %q, want %q", got, reason)
	}
}

func TestServer_Reserve(t *testing.T) {
	t.Run("success maps status and allocations", func(t *testing.T) {
		stub := &reservationsStub{result: domain.ReservationResult{
			ID:     "order-1",
			Status: domain.ReservationReserved,
			Allocations: []domain.Allocation{
				{SKUID: "sku-a", WarehouseID: 7, Quantity: 2},
			},
		}}
		resp, err := newReservationServer(stub).Reserve(context.Background(), reserveRequest())
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if resp.GetStatus() != inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED {
			t.Errorf("status = %v, want RESERVED", resp.GetStatus())
		}
		if len(resp.GetAllocations()) != 1 {
			t.Fatalf("allocations = %+v, want 1", resp.GetAllocations())
		}
		a := resp.GetAllocations()[0]
		if a.GetSkuId() != "sku-a" || a.GetWarehouseId() != "7" || a.GetQuantity() != 2 {
			t.Errorf("allocation = %+v, want sku-a wh=7 qty=2", a)
		}
		if stub.gotReq.ID != "order-1" || stub.gotReq.OrderID != "order-1" {
			t.Errorf("logic received %+v", stub.gotReq)
		}
	})

	t.Run("matching caller hash is accepted", func(t *testing.T) {
		req := reserveRequest()
		req.RequestHash = domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 2}}, "")
		stub := &reservationsStub{result: domain.ReservationResult{Status: domain.ReservationReserved}}
		if _, err := newReservationServer(stub).Reserve(context.Background(), req); err != nil {
			t.Fatalf("reserve with matching hash: %v", err)
		}
	})

	t.Run("caller hash drift -> VALIDATION_ERROR", func(t *testing.T) {
		req := reserveRequest()
		req.RequestHash = "deadbeef"
		_, err := newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
		wantValidationError(t, err)
	})

	t.Run("hash check canonicalizes duplicate lines like the logic layer", func(t *testing.T) {
		// Two lines of the same SKU: the caller hashed the aggregated form,
		// so the transport must aggregate before comparing.
		req := reserveRequest()
		req.Items = []*inventoryv1.ReservationItem{
			{SkuId: "sku-a", Quantity: 1},
			{SkuId: "sku-a", Quantity: 1},
		}
		req.RequestHash = domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 2}}, "")
		stub := &reservationsStub{result: domain.ReservationResult{Status: domain.ReservationReserved}}
		if _, err := newReservationServer(stub).Reserve(context.Background(), req); err != nil {
			t.Fatalf("reserve with aggregated hash: %v", err)
		}
	})

	t.Run("invalid ids -> VALIDATION_ERROR", func(t *testing.T) {
		for name, mut := range map[string]func(*inventoryv1.ReserveRequest){
			"empty reservation_id":   func(r *inventoryv1.ReserveRequest) { r.ReservationId = "" },
			"hostile reservation_id": func(r *inventoryv1.ReserveRequest) { r.ReservationId = "id with space" },
			// 187 chars: res:<id>:<sku64> would need 256 > VARCHAR(255).
			"oversized reservation_id": func(r *inventoryv1.ReserveRequest) { r.ReservationId = strings.Repeat("x", 187) },
			"empty order_id":           func(r *inventoryv1.ReserveRequest) { r.OrderId = "" },
			"hostile order_id":         func(r *inventoryv1.ReserveRequest) { r.OrderId = "ord\n" },
		} {
			req := reserveRequest()
			mut(req)
			_, err := newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: code = %v, want InvalidArgument", name, status.Code(err))
			}
		}
	})

	t.Run("item bounds -> VALIDATION_ERROR", func(t *testing.T) {
		for name, items := range map[string][]*inventoryv1.ReservationItem{
			"empty items":        {},
			"zero quantity":      {{SkuId: "sku-a", Quantity: 0}},
			"quantity above cap": {{SkuId: "sku-a", Quantity: maxQuantity + 1}},
			"hostile sku":        {{SkuId: "sku'; --", Quantity: 1}},
		} {
			req := reserveRequest()
			req.Items = items
			_, err := newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
			wantValidationError(t, err)
			_ = name
		}
		req := reserveRequest()
		req.Items = make([]*inventoryv1.ReservationItem, maxCheckItems+1)
		for i := range req.Items {
			req.Items[i] = &inventoryv1.ReservationItem{SkuId: "sku-a", Quantity: 1}
		}
		_, err := newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
		wantValidationError(t, err)
	})

	t.Run("malformed expires_at -> VALIDATION_ERROR", func(t *testing.T) {
		req := reserveRequest()
		req.ExpiresAt = "tomorrow"
		_, err := newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
		wantValidationError(t, err)
	})

	t.Run("boundary 186-char reservation_id is accepted", func(t *testing.T) {
		req := reserveRequest()
		req.ReservationId = strings.Repeat("x", 186)
		stub := &reservationsStub{result: domain.ReservationResult{Status: domain.ReservationReserved}}
		if _, err := newReservationServer(stub).Reserve(context.Background(), req); err != nil {
			t.Fatalf("186-char id rejected: %v", err)
		}
	})

	t.Run("hash-ambiguous destination_region -> VALIDATION_ERROR, value not echoed", func(t *testing.T) {
		// "9|dest:x" could collide with a hash of different items if it ever
		// reached CanonicalHash; the charset gate must reject it first.
		req := reserveRequest()
		req.DestinationRegion = "9|dest:x"
		_, err := newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
		wantValidationError(t, err)
		if strings.Contains(err.Error(), "9|dest:x") {
			t.Errorf("error message echoes the hostile region: %v", err)
		}

		req = reserveRequest()
		req.DestinationRegion = "eu:west"
		_, err = newReservationServer(&reservationsStub{}).Reserve(context.Background(), req)
		wantValidationError(t, err) // ':' excluded too — it is the pair separator

		req = reserveRequest()
		req.DestinationRegion = "eu-west_1"
		stub := &reservationsStub{result: domain.ReservationResult{Status: domain.ReservationReserved}}
		if _, err := newReservationServer(stub).Reserve(context.Background(), req); err != nil {
			t.Fatalf("legitimate region rejected: %v", err)
		}
	})

	t.Run("insufficient stock -> FailedPrecondition INSUFFICIENT_STOCK", func(t *testing.T) {
		stub := &reservationsStub{err: &domain.InsufficientStockError{
			Shortages: []domain.Shortage{{SKUID: "sku-a", Requested: 2, ATP: 0}}}}
		_, err := newReservationServer(stub).Reserve(context.Background(), reserveRequest())
		wantReason(t, err, codes.FailedPrecondition, grpcx.ReasonInsufficientStock)
	})

	t.Run("idempotency conflict -> AlreadyExists IDEMPOTENCY_CONFLICT", func(t *testing.T) {
		stub := &reservationsStub{err: domain.ErrIdempotencyConflict}
		_, err := newReservationServer(stub).Reserve(context.Background(), reserveRequest())
		wantReason(t, err, codes.AlreadyExists, grpcx.ReasonIdempotencyConflict)
	})

	t.Run("concurrency conflict -> Aborted CONCURRENCY_CONFLICT", func(t *testing.T) {
		stub := &reservationsStub{err: domain.ErrConcurrencyConflict}
		_, err := newReservationServer(stub).Reserve(context.Background(), reserveRequest())
		wantReason(t, err, codes.Aborted, grpcx.ReasonConcurrencyConflict)
	})

	t.Run("storage failure -> Unavailable DEPENDENCY_UNAVAILABLE", func(t *testing.T) {
		stub := &reservationsStub{err: errors.New("db down")}
		_, err := newReservationServer(stub).Reserve(context.Background(), reserveRequest())
		wantReason(t, err, codes.Unavailable, grpcx.ReasonDependencyUnavailable)
	})

	t.Run("canceled context -> Canceled without reason detail", func(t *testing.T) {
		stub := &reservationsStub{err: context.Canceled}
		_, err := newReservationServer(stub).Reserve(context.Background(), reserveRequest())
		if status.Code(err) != codes.Canceled {
			t.Fatalf("code = %v, want Canceled", status.Code(err))
		}
		if got := grpcx.Reason(err); got != "" {
			t.Fatalf("reason = %q, want none", got)
		}
	})
}

func TestServer_Release(t *testing.T) {
	t.Run("success maps the status and passes the reason", func(t *testing.T) {
		stub := &reservationsStub{status: domain.ReservationReleased}
		resp, err := newReservationServer(stub).Release(context.Background(),
			&inventoryv1.ReleaseRequest{ReservationId: "order-1", Reason: "saga_compensation"})
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if resp.GetStatus() != inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED {
			t.Errorf("status = %v, want RELEASED", resp.GetStatus())
		}
		if stub.gotReason != "saga_compensation" {
			t.Errorf("reason = %q, want saga_compensation", stub.gotReason)
		}
	})

	t.Run("free-form reason -> VALIDATION_ERROR", func(t *testing.T) {
		for _, reason := range []string{"has spaces here", strings.Repeat("x", 65)} {
			_, err := newReservationServer(&reservationsStub{}).Release(context.Background(),
				&inventoryv1.ReleaseRequest{ReservationId: "order-1", Reason: reason})
			wantValidationError(t, err)
		}
	})

	t.Run("invalid id -> VALIDATION_ERROR", func(t *testing.T) {
		_, err := newReservationServer(&reservationsStub{}).Release(context.Background(),
			&inventoryv1.ReleaseRequest{ReservationId: ""})
		wantValidationError(t, err)
	})

	t.Run("release of committed -> FailedPrecondition INVALID_TRANSITION", func(t *testing.T) {
		stub := &reservationsStub{err: domain.ErrInvalidTransition}
		_, err := newReservationServer(stub).Release(context.Background(),
			&inventoryv1.ReleaseRequest{ReservationId: "order-1"})
		wantReason(t, err, codes.FailedPrecondition, grpcx.ReasonInvalidTransition)
	})
}

func TestServer_Commit(t *testing.T) {
	t.Run("success maps the status", func(t *testing.T) {
		stub := &reservationsStub{status: domain.ReservationCommitted}
		resp, err := newReservationServer(stub).Commit(context.Background(),
			&inventoryv1.CommitRequest{ReservationId: "order-1"})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if resp.GetStatus() != inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED {
			t.Errorf("status = %v, want COMMITTED", resp.GetStatus())
		}
	})

	t.Run("invalid id -> VALIDATION_ERROR", func(t *testing.T) {
		_, err := newReservationServer(&reservationsStub{}).Commit(context.Background(),
			&inventoryv1.CommitRequest{ReservationId: "bad id"})
		wantValidationError(t, err)
	})

	t.Run("missing reservation -> NotFound NOT_FOUND", func(t *testing.T) {
		stub := &reservationsStub{err: domain.ErrReservationNotFound}
		_, err := newReservationServer(stub).Commit(context.Background(),
			&inventoryv1.CommitRequest{ReservationId: "order-ghost"})
		wantReason(t, err, codes.NotFound, grpcx.ReasonNotFound)
	})

	t.Run("commit of released -> FailedPrecondition INVALID_TRANSITION", func(t *testing.T) {
		stub := &reservationsStub{err: domain.ErrInvalidTransition}
		_, err := newReservationServer(stub).Commit(context.Background(),
			&inventoryv1.CommitRequest{ReservationId: "order-1"})
		wantReason(t, err, codes.FailedPrecondition, grpcx.ReasonInvalidTransition)
	})
}

func TestToProtoReservationStatus(t *testing.T) {
	cases := map[string]inventoryv1.ReservationStatus{
		domain.ReservationReserved:  inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED,
		domain.ReservationCommitted: inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED,
		domain.ReservationReleased:  inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED,
		domain.ReservationExpired:   inventoryv1.ReservationStatus_RESERVATION_STATUS_EXPIRED,
		"unknown-vocabulary":        inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED,
	}
	for in, want := range cases {
		if got := toProtoReservationStatus(in); got != want {
			t.Errorf("toProtoReservationStatus(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestServer_GetReservation(t *testing.T) {
	t.Run("success maps header, lines, and timestamps", func(t *testing.T) {
		stub := &reservationsStub{res: domain.Reservation{
			ID:      "order-1",
			OrderID: "order-1",
			Status:  domain.ReservationCommitted,
			Allocations: []domain.Allocation{
				{SKUID: "sku-a", WarehouseID: 1, Quantity: 2},
			},
			CreatedAt: "2026-07-23T00:00:00Z",
			UpdatedAt: "2026-07-23T00:05:00Z",
			ExpiresAt: "2026-07-24T00:00:00Z",
		}}
		resp, err := newReservationServer(stub).GetReservation(context.Background(),
			&inventoryv1.GetReservationRequest{ReservationId: "order-1"})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		r := resp.GetReservation()
		if r.GetId() != "order-1" || r.GetOrderId() != "order-1" ||
			r.GetStatus() != inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED {
			t.Errorf("reservation = %+v", r)
		}
		if len(r.GetAllocations()) != 1 || r.GetAllocations()[0].GetWarehouseId() != "1" {
			t.Errorf("allocations = %+v", r.GetAllocations())
		}
		if r.GetCreatedAt() == "" || r.GetUpdatedAt() == "" || r.GetExpiresAt() == "" {
			t.Errorf("timestamps missing: %+v", r)
		}
	})

	t.Run("missing reservation -> NotFound NOT_FOUND", func(t *testing.T) {
		stub := &reservationsStub{err: domain.ErrReservationNotFound}
		_, err := newReservationServer(stub).GetReservation(context.Background(),
			&inventoryv1.GetReservationRequest{ReservationId: "order-ghost"})
		wantReason(t, err, codes.NotFound, grpcx.ReasonNotFound)
	})

	t.Run("invalid id -> VALIDATION_ERROR", func(t *testing.T) {
		_, err := newReservationServer(&reservationsStub{}).GetReservation(context.Background(),
			&inventoryv1.GetReservationRequest{ReservationId: ""})
		wantValidationError(t, err)
	})
}
