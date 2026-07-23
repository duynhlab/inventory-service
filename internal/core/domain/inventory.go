// Package domain holds the inventory core types shared by the logic and
// repository layers (RFC-0021). Business invariants live in repository
// transactions backed by the schema CHECKs — never in transport handlers.
package domain

import (
	"context"
	"errors"
	"fmt"
)

// Movement types recorded in the append-only ledger. They mirror the
// migration's CHECK constraint — adding a type means a migration first.
const (
	MovementReceive        = "RECEIVE"
	MovementAdjust         = "ADJUST"
	MovementSetSafetyStock = "SET_SAFETY_STOCK"
	MovementReserve        = "RESERVE"
	MovementRelease        = "RELEASE"
	MovementSaleCommitted  = "SALE_COMMITTED"
	MovementReturn         = "RETURN"
)

// ErrCommandConflict is returned when a stock command's command_id was already
// applied with a different effect — the caller sent two different commands
// under one idempotency key.
var ErrCommandConflict = errors.New("command_id already applied with a different payload")

// ErrInsufficientOnHand is returned when an adjustment would drive a balance
// below zero (or below reserved) — surfaced as a business rejection, backed
// by the schema CHECKs.
var ErrInsufficientOnHand = errors.New("adjustment would violate balance invariants")

// ErrBalanceNotFound is returned when a command targets a (sku, warehouse)
// pair with no balance row — stock that was never received cannot be adjusted.
var ErrBalanceNotFound = errors.New("no balance row for sku/warehouse")

// StockCommand is an idempotent admin mutation of one (sku, warehouse)
// balance. CommandID is the natural idempotency key: replaying the same
// command is a no-op success; every applied command writes exactly one
// movement row.
type StockCommand struct {
	CommandID   string
	SKUID       string
	WarehouseID int64
	// Quantity semantics depend on the operation: ReceiveStock requires > 0
	// units to add; AdjustOnHand is a signed delta; SetSafetyStock is the new
	// absolute safety-stock level (>= 0).
	Quantity int64
	Reason   string
	Actor    string
}

// Validate rejects commands whose identity fields cannot claim a movement row
// correctly: an empty CommandID would make unrelated commands collide on the
// idempotency key (the second silently swallowed as a "replay"), and the
// length bounds mirror the schema varchar limits so oversized ids fail here
// instead of as opaque errors mid-transaction.
func (c StockCommand) Validate() error {
	switch {
	case c.CommandID == "":
		return errors.New("command_id must not be empty")
	case len(c.CommandID) > 255:
		return fmt.Errorf("command_id must be <= 255 chars, got %d", len(c.CommandID))
	case c.SKUID == "":
		return errors.New("sku_id must not be empty")
	case len(c.SKUID) > 64:
		return fmt.Errorf("sku_id must be <= 64 chars, got %d", len(c.SKUID))
	case c.WarehouseID <= 0:
		return fmt.Errorf("warehouse_id must be > 0, got %d", c.WarehouseID)
	case len(c.Actor) > 64:
		return fmt.Errorf("actor must be <= 64 chars, got %d", len(c.Actor))
	}
	return nil
}

// StockCommander is the admin-command port (RFC-0021 P1-3). Implementations
// must be transactional: the movement-ledger insert claims the command_id and
// the balance change commits atomically with it.
type StockCommander interface {
	// ReceiveStock adds Quantity units of on-hand stock, creating the balance
	// row on first receipt. Replay reports applied=false.
	ReceiveStock(ctx context.Context, cmd StockCommand) (applied bool, err error)
	// AdjustOnHand applies a signed on-hand delta (shrinkage, correction).
	// Violating the balance invariants returns ErrInsufficientOnHand.
	AdjustOnHand(ctx context.Context, cmd StockCommand) (applied bool, err error)
	// SetSafetyStock sets the absolute safety-stock level.
	SetSafetyStock(ctx context.Context, cmd StockCommand) (applied bool, err error)
}
