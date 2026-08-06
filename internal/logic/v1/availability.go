// Package v1 holds the business logic for inventory, version 1. The gRPC
// transport (internal/grpc/v1) is a thin adapter over this layer so future
// transports share the same rules.
package v1

import (
	"context"
	"errors"
	"fmt"
)

// lowStockThreshold classifies ATP below this bound as LOW_STOCK: enough to
// sell, low enough that browsing surfaces should nudge urgency.
const lowStockThreshold = 5

// AvailabilityStatus classifies a SKU's availability for surfaces that don't
// need exact quantities. It mirrors inventory.v1's AvailabilityStatus without
// importing the proto into the logic layer.
type AvailabilityStatus int

const (
	// StatusUnknown means inventory could not answer for this SKU: it is
	// untracked — no balance row in ANY warehouse, active or not. Per
	// contract it is never fabricated into OUT_OF_STOCK. A tracked SKU whose
	// only stock sits in inactive warehouses answers OUT_OF_STOCK instead:
	// inventory knows it, and nothing is promisable.
	StatusUnknown AvailabilityStatus = iota
	StatusInStock
	StatusLowStock
	StatusOutOfStock
)

// SkuAvailability is one per-SKU answer of BatchGetAvailability.
type SkuAvailability struct {
	SKUID  string
	Status AvailabilityStatus
	ATP    int64
}

// CheckItem is one basket line of CheckAvailability.
type CheckItem struct {
	SKUID    string
	Quantity int64
}

// ShortageLine names a line the chosen warehouse cannot fulfill and by how
// much.
type ShortageLine struct {
	SKUID     string
	Requested int64
	ATP       int64
}

// CheckResult is the whole-basket answer. Shortages and UnknownSKUIDs are both
// empty when CanFulfill is true.
type CheckResult struct {
	CanFulfill bool
	// Shortages are lines this service TRACKS and cannot fulfill — a business
	// answer carrying a real quantity.
	Shortages []ShortageLine
	// UnknownSKUIDs are requested SKUs with no balance row in any warehouse.
	//
	// They are NOT reported as shortages, and that distinction is the point: a
	// shortage asserts "there are N of these", which this service cannot claim
	// about a SKU it has never heard of. Reporting one as a zero-ATP shortage
	// told the shopper an item was unavailable when the real problem was missing
	// data — and it is missing data that is now the realistic case, since the
	// phase-2 backfill from product was retired with the column it copied.
	UnknownSKUIDs []string
}

// AvailabilityReader is the repository dependency of AvailabilityService.
// *repository.AvailabilityRepository satisfies it.
type AvailabilityReader interface {
	BatchATP(ctx context.Context, skuIDs []string) (map[string]int64, error)
	// ActiveWarehouseBalances also returns every active warehouse id in
	// ascending order, including warehouses holding no rows for these SKUs.
	ActiveWarehouseBalances(ctx context.Context, skuIDs []string) (map[int64]map[string]int64, []int64, error)
	TrackedSKUs(ctx context.Context, skuIDs []string) (map[string]bool, error)
}

// AvailabilityService implements the two read RPCs of inventory.v1
// (RFC-0021 P1-4): per-SKU availability and the whole-basket check.
type AvailabilityService struct {
	repo AvailabilityReader
}

// NewAvailabilityService creates the availability logic service.
func NewAvailabilityService(repo AvailabilityReader) *AvailabilityService {
	return &AvailabilityService{repo: repo}
}

// BatchGetAvailability returns status and ATP for every requested SKU, in
// request order, without ever erroring the batch for a bad id. A SKU absent
// from the active-warehouse ATP map is disambiguated via TrackedSKUs:
// tracked (a balance row exists somewhere, e.g. only in a deactivated
// warehouse) → OUT_OF_STOCK with ATP 0, because inventory knows the SKU and
// nothing is promisable; untracked → UNKNOWN with ATP 0, because inventory
// cannot answer for an id it has never seen.
func (s *AvailabilityService) BatchGetAvailability(ctx context.Context, skuIDs []string) ([]SkuAvailability, error) {
	ctx, span := startLogicSpan(ctx, opBatchAvailability)
	defer span.End()

	atps, err := s.repo.BatchATP(ctx, skuIDs)
	if err != nil {
		recordSpanError(ctx, opBatchAvailability, err)
		return nil, fmt.Errorf("batch get availability: %w", err)
	}

	// Only SKUs missing from the active-ATP map need the tracked/untracked
	// lookup; the common all-tracked batch costs one query.
	missing := make([]string, 0)
	for _, id := range skuIDs {
		if _, ok := atps[id]; !ok {
			missing = append(missing, id)
		}
	}
	tracked := map[string]bool{}
	if len(missing) > 0 {
		if tracked, err = s.repo.TrackedSKUs(ctx, missing); err != nil {
			recordSpanError(ctx, opBatchAvailability, err)
			return nil, fmt.Errorf("batch get availability: %w", err)
		}
	}

	out := make([]SkuAvailability, 0, len(skuIDs))
	for _, id := range skuIDs {
		atp, ok := atps[id]
		switch {
		case ok:
			out = append(out, SkuAvailability{SKUID: id, Status: statusFor(atp), ATP: atp})
		case tracked[id]:
			out = append(out, SkuAvailability{SKUID: id, Status: StatusOutOfStock})
		default:
			out = append(out, SkuAvailability{SKUID: id, Status: StatusUnknown})
		}
	}
	setSpanOutcome(ctx, outcomeOK)
	return out, nil
}

// statusFor maps an ATP quantity to its bounded status.
func statusFor(atp int64) AvailabilityStatus {
	switch {
	case atp <= 0:
		return StatusOutOfStock
	case atp < lowStockThreshold:
		return StatusLowStock
	default:
		return StatusInStock
	}
}

// CheckAvailability answers "can this whole basket be fulfilled?": true when
// at least one active warehouse's ATP satisfies every line (v1 allocates a
// whole order from one warehouse, so summed cross-warehouse stock does not
// count). Warehouses are evaluated in ascending id so the answer is
// deterministic under equal stock; a warehouse with no balance rows simply
// cannot fulfill (every quantity is > 0). When none fulfills, shortages are
// computed against the lowest-id ACTIVE warehouse overall — the same one
// Reserve would try first — even when it holds no rows for these SKUs; a SKU
// with no balance row there counts as ATP 0. When NO active warehouse exists
// the basket is unfulfillable by definition: can_fulfill false with every
// line short at ATP 0.
func (s *AvailabilityService) CheckAvailability(ctx context.Context, items []CheckItem) (*CheckResult, error) {
	ctx, span := startLogicSpan(ctx, opCheckAvailability)
	defer span.End()

	skuIDs := make([]string, 0, len(items))
	for _, it := range items {
		skuIDs = append(skuIDs, it.SKUID)
	}

	byWarehouse, activeIDs, err := s.repo.ActiveWarehouseBalances(ctx, skuIDs)
	if err != nil {
		// A canceled request is the caller hanging up, not a check outcome —
		// counting it as error would let client churn masquerade as DB
		// trouble on the on-call dashboard. recordSpanError applies the same
		// canceled skip to the span.
		if !errors.Is(err, context.Canceled) {
			recordCheck(ctx, outcomeError)
		}
		recordSpanError(ctx, opCheckAvailability, err)
		return nil, fmt.Errorf("check availability: %w", err)
	}

	for _, id := range activeIDs {
		if fulfills(byWarehouse[id], items) {
			recordCheck(ctx, outcomeFulfillable)
			setSpanOutcome(ctx, outcomeFulfillable)
			return &CheckResult{CanFulfill: true}, nil
		}
	}

	// With no active warehouse at all the nil base yields ATP 0 per line.
	var base map[string]int64
	if len(activeIDs) > 0 {
		base = byWarehouse[activeIDs[0]]
	}

	// Absent from the chosen warehouse's map means one of two very different
	// things, and the same lookup BatchGetAvailability already uses tells them
	// apart: tracked somewhere (a real zero here) versus not tracked at all.
	// Only ask about the lines that are actually short, so a fulfillable-shaped
	// basket costs no extra query.
	shortIDs := make([]string, 0, len(items))
	for _, it := range items {
		if base[it.SKUID] < it.Quantity {
			shortIDs = append(shortIDs, it.SKUID)
		}
	}
	tracked := map[string]bool{}
	if len(shortIDs) > 0 {
		if tracked, err = s.repo.TrackedSKUs(ctx, shortIDs); err != nil {
			// Fail the CALL rather than guess. Answering "shortage" here would
			// name quantities we could not verify, and answering "unknown"
			// would blame the data for a database problem.
			if !errors.Is(err, context.Canceled) {
				recordCheck(ctx, outcomeError)
			}
			recordSpanError(ctx, opCheckAvailability, err)
			return nil, fmt.Errorf("check availability: tracked lookup: %w", err)
		}
	}

	shortages := make([]ShortageLine, 0, len(shortIDs))
	unknown := make([]string, 0)
	for _, it := range items {
		atp := base[it.SKUID]
		if atp >= it.Quantity {
			continue
		}
		if _, hasATP := base[it.SKUID]; hasATP || tracked[it.SKUID] {
			shortages = append(shortages, ShortageLine{SKUID: it.SKUID, Requested: it.Quantity, ATP: atp})
			continue
		}
		unknown = append(unknown, it.SKUID)
	}

	// An unknown SKU dominates the outcome label: the basket's blocking reason
	// is the one an operator must act on, and a data gap outranks a stockout.
	outcome := outcomeShortage
	if len(unknown) > 0 {
		outcome = outcomeUnknownSKU
	}
	recordCheck(ctx, outcome)
	setSpanOutcome(ctx, outcome)
	return &CheckResult{Shortages: shortages, UnknownSKUIDs: unknown}, nil
}

// fulfills reports whether one warehouse's ATP satisfies every basket line.
func fulfills(atp map[string]int64, items []CheckItem) bool {
	for _, it := range items {
		if atp[it.SKUID] < it.Quantity {
			return false
		}
	}
	return true
}
