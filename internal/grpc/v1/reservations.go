package v1

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/duynhlab/inventory-service/internal/core/domain"
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// Reservations is the logic-layer dependency for the four write RPCs.
// *logicv1.ReservationService satisfies it.
type Reservations interface {
	Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error)
	Release(ctx context.Context, id, reason string) (string, error)
	Commit(ctx context.Context, id string) (string, error)
	GetReservation(ctx context.Context, id string) (domain.Reservation, error)
}

// Reserve places an all-or-nothing hold on stock for an order (saga step).
// The canonical request hash is always computed server-side; a caller-provided
// request_hash that disagrees is rejected as VALIDATION_ERROR — it means the
// caller's canonicalization drifted from the contract, and accepting it would
// let a divergent retry masquerade as a replay.
func (s *Server) Reserve(
	ctx context.Context,
	req *inventoryv1.ReserveRequest,
) (*inventoryv1.ReserveResponse, error) {
	if !validReservationID(req.GetReservationId()) {
		return nil, errInvalidReservationID(fieldReservationID)
	}
	if !validReservationID(req.GetOrderId()) {
		return nil, errInvalidReservationID("order_id")
	}
	if !validDestinationRegion(req.GetDestinationRegion()) {
		return nil, errInvalidDestinationRegion()
	}
	items, err := reservationLines(req.GetItems())
	if err != nil {
		return nil, err
	}
	if e := req.GetExpiresAt(); e != "" {
		if _, perr := time.Parse(time.RFC3339, e); perr != nil {
			return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
				"expires_at must be RFC-3339", nil)
		}
	}
	canonical := domain.CanonicalHash(domain.AggregateLines(items), req.GetDestinationRegion())
	if h := req.GetRequestHash(); h != "" && h != canonical {
		return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
			"request_hash does not match the canonical server-side hash", nil)
	}

	res, err := s.reservations.Reserve(ctx, domain.ReservationRequest{
		ID:                req.GetReservationId(),
		OrderID:           req.GetOrderId(),
		Items:             items,
		DestinationRegion: req.GetDestinationRegion(),
		ExpiresAt:         req.GetExpiresAt(),
	})
	if err != nil {
		return nil, s.reservationError("Reserve", err)
	}
	allocations := make([]*inventoryv1.Allocation, 0, len(res.Allocations))
	for _, a := range res.Allocations {
		allocations = append(allocations, &inventoryv1.Allocation{
			SkuId:       a.SKUID,
			WarehouseId: strconv.FormatInt(a.WarehouseID, 10),
			Quantity:    a.Quantity,
		})
	}
	return &inventoryv1.ReserveResponse{
		ReservationId: res.ID,
		Status:        toProtoReservationStatus(res.Status),
		Allocations:   allocations,
	}, nil
}

// Release returns a reservation's stock (saga compensation, pre-pivot).
func (s *Server) Release(
	ctx context.Context,
	req *inventoryv1.ReleaseRequest,
) (*inventoryv1.ReleaseResponse, error) {
	if !validReservationID(req.GetReservationId()) {
		return nil, errInvalidReservationID(fieldReservationID)
	}
	if !releaseReasonRe.MatchString(req.GetReason()) {
		return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
			"reason must match [A-Za-z0-9_.-]{0,64}", nil)
	}
	status, err := s.reservations.Release(ctx, req.GetReservationId(), req.GetReason())
	if err != nil {
		return nil, s.reservationError("Release", err)
	}
	return &inventoryv1.ReleaseResponse{
		ReservationId: req.GetReservationId(),
		Status:        toProtoReservationStatus(status),
	}, nil
}

// Commit converts a reservation into a sale (post-pivot mandatory forward).
func (s *Server) Commit(
	ctx context.Context,
	req *inventoryv1.CommitRequest,
) (*inventoryv1.CommitResponse, error) {
	if !validReservationID(req.GetReservationId()) {
		return nil, errInvalidReservationID(fieldReservationID)
	}
	status, err := s.reservations.Commit(ctx, req.GetReservationId())
	if err != nil {
		return nil, s.reservationError("Commit", err)
	}
	return &inventoryv1.CommitResponse{
		ReservationId: req.GetReservationId(),
		Status:        toProtoReservationStatus(status),
	}, nil
}

// GetReservation returns a reservation and its lines — used by the
// order-domain reconciler and operators.
func (s *Server) GetReservation(
	ctx context.Context,
	req *inventoryv1.GetReservationRequest,
) (*inventoryv1.GetReservationResponse, error) {
	if !validReservationID(req.GetReservationId()) {
		return nil, errInvalidReservationID(fieldReservationID)
	}
	res, err := s.reservations.GetReservation(ctx, req.GetReservationId())
	if err != nil {
		return nil, s.reservationError("GetReservation", err)
	}
	allocations := make([]*inventoryv1.Allocation, 0, len(res.Allocations))
	for _, a := range res.Allocations {
		allocations = append(allocations, &inventoryv1.Allocation{
			SkuId:       a.SKUID,
			WarehouseId: strconv.FormatInt(a.WarehouseID, 10),
			Quantity:    a.Quantity,
		})
	}
	return &inventoryv1.GetReservationResponse{
		Reservation: &inventoryv1.Reservation{
			Id:          res.ID,
			OrderId:     res.OrderID,
			Status:      toProtoReservationStatus(res.Status),
			Allocations: allocations,
			CreatedAt:   res.CreatedAt,
			UpdatedAt:   res.UpdatedAt,
			ExpiresAt:   res.ExpiresAt,
		},
	}, nil
}

// reservationLines validates and converts the request items. No aggregation
// here beyond the hash check — the logic layer owns it — but bounds and
// charset are enforced before anything reaches state-mutating SQL.
func reservationLines(items []*inventoryv1.ReservationItem) ([]domain.Line, error) {
	n := len(items)
	if n == 0 || n > maxCheckItems {
		return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
			fmt.Sprintf("items must contain 1..%d lines", maxCheckItems), nil)
	}
	out := make([]domain.Line, 0, n)
	for _, it := range items {
		q := it.GetQuantity()
		if q <= 0 || q > maxQuantity {
			return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
				fmt.Sprintf("item quantity must be 1..%d", maxQuantity), nil)
		}
		if !validSKUID(it.GetSkuId()) {
			return nil, errInvalidSKUID()
		}
		out = append(out, domain.Line{SKUID: it.GetSkuId(), Quantity: q})
	}
	return out, nil
}

// reservationError maps a domain failure onto the wire contract. Business
// rejections carry their stable grpcx reason; anything unrecognized is a
// storage failure and fails closed as retryable DEPENDENCY_UNAVAILABLE.
func (s *Server) reservationError(rpc string, err error) error {
	switch {
	case errors.Is(err, domain.ErrUnknownSKU):
		// Before ErrInsufficientStock arm-order matters conceptually, but the
		// errors are distinct types so either order is correct; the reason
		// token was reserved in the contract vocabulary from day one and
		// finally has a producer. The ids stay in logs/spans, not the message.
		return grpcx.ErrorWithReason(codes.FailedPrecondition, grpcx.ReasonSKUNotFound,
			"sku not tracked by inventory", nil)
	case errors.Is(err, domain.ErrInsufficientStock):
		return grpcx.ErrorWithReason(codes.FailedPrecondition, grpcx.ReasonInsufficientStock,
			"insufficient stock to reserve", nil)
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return grpcx.ErrorWithReason(codes.AlreadyExists, grpcx.ReasonIdempotencyConflict,
			"reservation or order id reused with a different request", nil)
	case errors.Is(err, domain.ErrInvalidTransition):
		return grpcx.ErrorWithReason(codes.FailedPrecondition, grpcx.ReasonInvalidTransition,
			"reservation status does not allow this transition", nil)
	case errors.Is(err, domain.ErrReservationNotFound):
		return grpcx.ErrorWithReason(codes.NotFound, grpcx.ReasonNotFound,
			"reservation not found", nil)
	case errors.Is(err, domain.ErrConcurrencyConflict):
		return grpcx.ErrorWithReason(codes.Aborted, grpcx.ReasonConcurrencyConflict,
			"concurrent transaction conflict, retry", nil)
	default:
		return s.failClosed(rpc, err)
	}
}

// toProtoReservationStatus maps the persisted status vocabulary to the wire
// enum.
func toProtoReservationStatus(status string) inventoryv1.ReservationStatus {
	switch status {
	case domain.ReservationReserved:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED
	case domain.ReservationCommitted:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED
	case domain.ReservationReleased:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED
	case domain.ReservationExpired:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_EXPIRED
	default:
		return inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED
	}
}
