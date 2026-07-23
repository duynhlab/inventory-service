// Package v1 implements the gRPC transport for inventory, version 1. It is a
// thin adapter over the logic layer (internal/logic/v1) so future transports
// share the same business rules.
//
// RFC-0021 P1-4 implements the two read RPCs (BatchGetAvailability,
// CheckAvailability); P1-5 adds the four write RPCs (Reserve, Release,
// Commit, GetReservation) — the full InventoryService surface is served.
package v1

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	logicv1 "github.com/duynhlab/inventory-service/internal/logic/v1"
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// Batch bounds: oversized requests fail VALIDATION_ERROR (per the proto
// contract) instead of turning one read into an unbounded table scan.
const (
	maxBatchSkuIDs = 200
	maxCheckItems  = 100
)

// Availability is the logic-layer dependency the gRPC server needs.
// *logicv1.AvailabilityService satisfies it.
type Availability interface {
	BatchGetAvailability(ctx context.Context, skuIDs []string) ([]logicv1.SkuAvailability, error)
	CheckAvailability(ctx context.Context, items []logicv1.CheckItem) (*logicv1.CheckResult, error)
}

// Server implements inventoryv1.InventoryServiceServer.
type Server struct {
	inventoryv1.UnimplementedInventoryServiceServer

	availability Availability
	reservations Reservations
	logger       *zap.Logger
}

// NewServer creates the gRPC InventoryService server backed by the
// availability and reservation logic services.
func NewServer(availability Availability, reservations Reservations, logger *zap.Logger) *Server {
	return &Server{availability: availability, reservations: reservations, logger: logger}
}

// failClosed translates a logic-layer failure into a wire status. The real
// error is logged here — the wire message stays sanitized (client-safe, no
// SQL or connection detail) — so operators keep the root cause without
// leaking it to callers. A canceled request propagates as codes.Canceled
// with no reason detail: the caller hung up; retry classification is theirs.
func (s *Server) failClosed(rpc string, err error) error {
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	s.logger.Error("Inventory RPC failed", zap.String("rpc", rpc), zap.Error(err))
	// A storage failure is retryable for callers — never a business "no".
	return grpcx.ErrorWithReason(codes.Unavailable, grpcx.ReasonDependencyUnavailable,
		rpc+" failed", nil)
}

// BatchGetAvailability returns per-SKU availability for product pages and
// checkout snapshots. Unknown SKUs come back AVAILABILITY_STATUS_UNKNOWN
// rather than erroring the batch. destination_region is accepted but unused
// in v1: there is a single default warehouse, so region filtering lands with
// the multi-warehouse model.
func (s *Server) BatchGetAvailability(
	ctx context.Context,
	req *inventoryv1.BatchGetAvailabilityRequest,
) (*inventoryv1.BatchGetAvailabilityResponse, error) {
	n := len(req.GetSkuIds())
	if n == 0 || n > maxBatchSkuIDs {
		return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
			fmt.Sprintf("sku_ids must contain 1..%d entries", maxBatchSkuIDs), nil)
	}
	for _, id := range req.GetSkuIds() {
		if !validSKUID(id) {
			return nil, errInvalidSKUID()
		}
	}

	availabilities, err := s.availability.BatchGetAvailability(ctx, req.GetSkuIds())
	if err != nil {
		return nil, s.failClosed("BatchGetAvailability", err)
	}

	out := make([]*inventoryv1.SkuAvailability, 0, len(availabilities))
	for _, a := range availabilities {
		out = append(out, &inventoryv1.SkuAvailability{
			SkuId:              a.SKUID,
			Status:             toProtoStatus(a.Status),
			AvailableToPromise: a.ATP,
		})
	}
	return &inventoryv1.BatchGetAvailabilityResponse{Availabilities: out}, nil
}

// CheckAvailability answers "can this whole basket be fulfilled?" — the
// checkout revalidation gate (advisory only; Reserve is the correctness
// gate). destination_region is accepted but unused in v1: there is a single
// default warehouse, so region filtering lands with the multi-warehouse
// model.
func (s *Server) CheckAvailability(
	ctx context.Context,
	req *inventoryv1.CheckAvailabilityRequest,
) (*inventoryv1.CheckAvailabilityResponse, error) {
	n := len(req.GetItems())
	if n == 0 || n > maxCheckItems {
		return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
			fmt.Sprintf("items must contain 1..%d lines", maxCheckItems), nil)
	}
	// Aggregate duplicate SKU lines so total demand is evaluated — two lines
	// of 3 against an ATP of 3 must answer "no", not check 3 twice. Order is
	// preserved for deterministic shortage reporting.
	items := make([]logicv1.CheckItem, 0, n)
	index := make(map[string]int, n)
	for _, it := range req.GetItems() {
		q := it.GetQuantity()
		if q <= 0 || q > maxQuantity {
			return nil, grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
				fmt.Sprintf("item quantity must be 1..%d", maxQuantity), nil)
		}
		if !validSKUID(it.GetSkuId()) {
			return nil, errInvalidSKUID()
		}
		if i, seen := index[it.GetSkuId()]; seen {
			items[i].Quantity += q
			continue
		}
		index[it.GetSkuId()] = len(items)
		items = append(items, logicv1.CheckItem{SKUID: it.GetSkuId(), Quantity: q})
	}

	result, err := s.availability.CheckAvailability(ctx, items)
	if err != nil {
		return nil, s.failClosed("CheckAvailability", err)
	}

	shortages := make([]*inventoryv1.Shortage, 0, len(result.Shortages))
	for _, sh := range result.Shortages {
		shortages = append(shortages, &inventoryv1.Shortage{
			SkuId:              sh.SKUID,
			Requested:          sh.Requested,
			AvailableToPromise: sh.ATP,
		})
	}
	return &inventoryv1.CheckAvailabilityResponse{
		CanFulfill: result.CanFulfill,
		Shortages:  shortages,
	}, nil
}

// toProtoStatus maps the logic-layer status to the wire enum.
func toProtoStatus(s logicv1.AvailabilityStatus) inventoryv1.AvailabilityStatus {
	switch s {
	case logicv1.StatusInStock:
		return inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_IN_STOCK
	case logicv1.StatusLowStock:
		return inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_LOW_STOCK
	case logicv1.StatusOutOfStock:
		return inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_OUT_OF_STOCK
	case logicv1.StatusUnknown:
		return inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_UNKNOWN
	default:
		return inventoryv1.AvailabilityStatus_AVAILABILITY_STATUS_UNKNOWN
	}
}
