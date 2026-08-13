package domain

import "errors"

// ErrInvalidCommand wraps a StockCommand.Validate failure on the protected
// command path, so transports can map "the request itself is bad" (400) apart
// from repository refusals (409) and infrastructure failures (500).
var ErrInvalidCommand = errors.New("invalid stock command")

// Read models for the protected Backoffice views (RFC-0023 slice A). These
// are projections of the schema for operator screens — no invariants live
// here; the write-side rules stay with StockCommand and the repositories.

// BalanceView is one (sku, warehouse) balance row. ATP is derived
// (on_hand - reserved) in SQL so every reader shows the same arithmetic the
// reservation path enforces; safety_stock is advisory and deliberately not
// subtracted (RFC-0021).
type BalanceView struct {
	SKUID       string
	WarehouseID int64
	OnHand      int64
	Reserved    int64
	SafetyStock int64
	ATP         int64
	UpdatedAt   string
}

// BalanceFilter narrows ListBalances. Zero values mean "no filter".
type BalanceFilter struct {
	SKUID       string
	WarehouseID int64
	// LowStockOnly keeps rows whose ATP has fallen to or below safety stock —
	// the operator's restock worklist (out-of-stock rows included).
	LowStockOnly bool
}

// MovementView is one append-only ledger row. Actor is empty for
// reservation-driven movements (their originator is reference_type/id).
type MovementView struct {
	ID            int64
	CommandID     string
	SKUID         string
	WarehouseID   int64
	Type          string
	OnHandDelta   int64
	ReservedDelta int64
	ReferenceType string
	ReferenceID   string
	Reason        string
	Actor         string
	CreatedAt     string
}

// MovementFilter narrows ListMovements. Zero values mean "no filter".
type MovementFilter struct {
	SKUID       string
	WarehouseID int64
}

// ReservationView is one reservation header row for operator inspection;
// line detail stays with the existing GetReservation gRPC read.
type ReservationView struct {
	ID        string
	OrderID   string
	Status    string
	ExpiresAt string
	CreatedAt string
	UpdatedAt string
}
