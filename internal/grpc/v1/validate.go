package v1

import (
	"regexp"

	"google.golang.org/grpc/codes"

	"github.com/duynhlab/pkg/grpcx"
)

// Input bounds shared by every RPC on this surface. The write path (P1-5
// Reserve/Release/Commit) inherits these so hostile identifiers are rejected
// before they reach state-mutating SQL, unique indexes, or error messages.
const (
	maxQuantity = 10_000
)

// skuIDRe bounds sku_id length and charset. Today sku_id = product id (an
// integer rendered as string); the pattern leaves room for a future
// human-opaque SKU scheme without admitting NUL bytes, whitespace, or
// megabyte strings into ANY($1) arrays, map keys, and echoes.
var skuIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// reservationIDRe bounds reservation_id and order_id. The extra `:` admits
// composite workflow ids; the bound keeps hostile strings out of primary
// keys, movement command_ids, and error messages. The length cap is 186, not
// the column's 255: every ledger row for a reservation carries a command_id
// `res:<id>:<sku>` (prefix 4 + id + separator 1 + sku up to 64) that must fit
// inventory_movements.command_id VARCHAR(255) — 186 + 69 = 255 exactly.
var reservationIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,186}$`)

// releaseReasonRe bounds the Release reason code: short, enumerable audit
// vocabulary — free-form text is rejected, empty is allowed.
var releaseReasonRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,64}$`)

// destinationRegionRe bounds destination_region. It deliberately excludes
// `:` and `|` — the CanonicalHash encoding joins `sku:qty` pairs with `|`
// and appends `|dest:<region>`, so a region containing those separators
// (e.g. "9|dest:x") could collide with a hash of different items. Rejecting
// the charset at the edge removes the ambiguity entirely.
var destinationRegionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)

// validSKUID reports whether id is safe to pass to the logic layer. Invalid
// IDs are a caller bug: the RPC returns VALIDATION_ERROR (non-retryable),
// never DEPENDENCY_UNAVAILABLE — a malformed request must not look like an
// outage or drive retry storms.
func validSKUID(id string) bool {
	return skuIDRe.MatchString(id)
}

func errInvalidSKUID() error {
	// Deliberately does not echo the offending ID — hostile strings are
	// never reflected into error messages.
	return grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
		"sku_ids entries must match [A-Za-z0-9._-]{1,64}", nil)
}

// fieldReservationID names the id field in validation errors — the field
// name is ours, never the hostile value.
const fieldReservationID = "reservation_id"

// validReservationID reports whether a reservation_id or order_id is safe.
func validReservationID(id string) bool {
	return reservationIDRe.MatchString(id)
}

func errInvalidReservationID(field string) error {
	// The field name is ours, never the hostile value.
	return grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
		field+" must match [A-Za-z0-9_.:-]{1,186}", nil)
}

// validDestinationRegion reports whether a destination_region is safe to
// feed into the canonical hash. The value is never echoed on rejection.
func validDestinationRegion(region string) bool {
	return destinationRegionRe.MatchString(region)
}

func errInvalidDestinationRegion() error {
	return grpcx.ErrorWithReason(codes.InvalidArgument, grpcx.ReasonValidationError,
		"destination_region must match [A-Za-z0-9_-]{0,64}", nil)
}
