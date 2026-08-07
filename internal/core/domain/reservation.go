package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Reservation FSM states persisted in inventory_reservations.status. RESERVED
// is the only non-terminal state. The vocabulary mirrors the schema CHECK —
// adding a state means a migration first.
const (
	ReservationReserved  = "reserved"
	ReservationCommitted = "committed"
	ReservationReleased  = "released"
	ReservationExpired   = "expired"
)

// ErrInsufficientStock is returned when no active warehouse can fulfill every
// line of a reservation. Wrap it in InsufficientStockError to carry the
// per-line shortage detail.
var ErrInsufficientStock = errors.New("insufficient stock to reserve")

// ErrUnknownSKU is returned when a requested SKU has no balance row in ANY
// warehouse — inventory has never heard of it. This is a DATA GAP, not a
// stockout: conflating the two filed seeding problems under
// INSUFFICIENT_STOCK, where a real customer-demand signal hid them
// (RFC-0021 deferred item 2). Wrap in UnknownSKUError for the ids.
var ErrUnknownSKU = errors.New("sku has no balance row in any warehouse")

// ErrIdempotencyConflict is returned when a reservation_id is replayed with a
// different request hash, or a different reservation_id reuses an order_id —
// silently accepting either would tell a caller their divergent request
// applied.
var ErrIdempotencyConflict = errors.New("reservation or order id reused with a different request")

// ErrInvalidTransition is returned when a reservation's current status does
// not allow the requested transition (release a committed sale, commit a
// released hold).
var ErrInvalidTransition = errors.New("reservation status does not allow this transition")

// ErrReservationNotFound is returned when the reservation id is unknown.
var ErrReservationNotFound = errors.New("reservation not found")

// ErrConcurrencyConflict is returned when Postgres aborts the transaction for
// concurrency reasons (serialization failure, deadlock). Retryable by design.
var ErrConcurrencyConflict = errors.New("concurrent transaction conflict")

// Shortage names a line that cannot be fulfilled and by how much.
type Shortage struct {
	SKUID     string
	Requested int64
	ATP       int64
}

// InsufficientStockError carries the shortage detail behind
// ErrInsufficientStock (matched via errors.Is).
type InsufficientStockError struct {
	Shortages []Shortage
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient stock for %d line(s)", len(e.Shortages))
}

func (e *InsufficientStockError) Unwrap() error { return ErrInsufficientStock }

// UnknownSKUError carries the untracked ids behind ErrUnknownSKU (matched via
// errors.Is). It takes precedence over a shortage in a mixed basket: a
// quantity claim about a SKU inventory does not know would be fabricated.
type UnknownSKUError struct {
	SKUIDs []string
}

func (e *UnknownSKUError) Error() string {
	return fmt.Sprintf("%d sku(s) have no balance row in any warehouse", len(e.SKUIDs))
}

func (e *UnknownSKUError) Unwrap() error { return ErrUnknownSKU }

// Line is one aggregated SKU/quantity pair of a reservation request.
type Line struct {
	SKUID    string
	Quantity int64
}

// ReservationRequest is the write-side input of Reserve. Items must be
// aggregated (one line per SKU) before hashing and allocation — see
// AggregateLines. ExpiresAt is an optional RFC-3339 timestamp,
// observability-only in v1.
type ReservationRequest struct {
	ID                string
	OrderID           string
	Items             []Line
	DestinationRegion string
	ExpiresAt         string
}

// Allocation reports where reserved stock is held. v1 allocates a whole
// reservation from one warehouse, so all allocations share WarehouseID.
type Allocation struct {
	SKUID       string
	WarehouseID int64
	Quantity    int64
}

// ReservationResult is the outcome of Reserve. Replayed marks an idempotent
// replay (the reservation already existed with the same request hash) so the
// caller can meter replays without re-deriving them.
type ReservationResult struct {
	ID          string
	Status      string
	Allocations []Allocation
	Replayed    bool
}

// Reservation is the read model of GetReservation: header plus lines.
// Timestamps are RFC-3339; ExpiresAt is empty when none was recorded.
type Reservation struct {
	ID          string
	OrderID     string
	Status      string
	Allocations []Allocation
	CreatedAt   string
	UpdatedAt   string
	ExpiresAt   string
}

// AggregateLines sums duplicate SKU lines, preserving first-seen order. Both
// the transport hash check and the repository allocation must see one line
// per SKU, or the canonical hash and the per-line balance math diverge.
func AggregateLines(items []Line) []Line {
	out := make([]Line, 0, len(items))
	index := make(map[string]int, len(items))
	for _, it := range items {
		if i, seen := index[it.SKUID]; seen {
			out[i].Quantity += it.Quantity
			continue
		}
		index[it.SKUID] = len(out)
		out = append(out, it)
	}
	return out
}

// CanonicalHash computes the server-side canonical request hash: sha256 hex
// of `sku:qty` pairs sorted by sku joined `|`, plus `|dest:<region>`. Items
// must already be aggregated (one line per SKU). Order-insensitive,
// quantity- and destination-sensitive by construction.
func CanonicalHash(items []Line, destinationRegion string) string {
	sorted := make([]Line, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SKUID < sorted[j].SKUID })

	var b strings.Builder
	for _, it := range sorted {
		b.WriteString(it.SKUID)
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(it.Quantity, 10))
		b.WriteByte('|')
	}
	b.WriteString("dest:")
	b.WriteString(destinationRegion)

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
