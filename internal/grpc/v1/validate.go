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
