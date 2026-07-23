// Package v1 implements the gRPC transport for inventory, version 1. It is a
// thin adapter over the logic layer (to be added) so future transports share
// the same business rules.
//
// Bootstrap skeleton (RFC-0021 P1-1): the server only embeds
// inventoryv1.UnimplementedInventoryServiceServer, so every RPC
// (BatchGetAvailability, CheckAvailability, Reserve, Release, Commit,
// GetReservation) answers codes.Unimplemented. P1-3..P1-5 implement them.
package v1

import (
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// Server implements inventoryv1.InventoryServiceServer.
type Server struct {
	inventoryv1.UnimplementedInventoryServiceServer
}

// NewServer creates the gRPC InventoryService server. It takes no dependencies
// yet; the logic service is injected here once P1-3 lands.
func NewServer() *Server {
	return &Server{}
}
