# inventory-service

Inventory microservice for the `duynhlab` platform — the **sole authority for
stock**: balances, reservations, allocations, and the stock-movement ledger
(RFC-0021, supersedes the product-owned stock surface).

Module path: `github.com/duynhlab/inventory-service`.

**Status: Bootstrap (RFC-0021 P1-1).** The skeleton serves operational HTTP on
`:8080` (`/health`, `/ready`) and the `inventory.v1.InventoryService` gRPC
server on `:9090` with every RPC answering `Unimplemented`. Schema (P1-2) and
RPC implementations (P1-3..P1-5) follow. Design record:
[duynhlab/homelab `docs/proposals/rfc/RFC-0021/`](https://github.com/duynhlab/homelab/tree/main/docs/proposals/rfc/RFC-0021).

## gRPC

- Listen address: `:9090` (`GRPC_PORT`, default `9090`)
- Service: `inventory.v1.InventoryService` (proto from `github.com/duynhlab/pkg/proto/inventory/v1`)
- Bootstrap via shared `github.com/duynhlab/pkg/grpcx` (`grpcx.NewServer`): OpenTelemetry interceptors, health, reflection
- All RPCs (`BatchGetAvailability`, `CheckAvailability`, `Reserve`, `Release`, `Commit`, `GetReservation`) currently return `Unimplemented`

There are no business HTTP routes: inventory is gRPC-only east-west.
Operational endpoints: `GET /health`, `GET /ready` (DB ping + drain-aware).

## Development

```bash
# Build
GOTOOLCHAIN=auto go build ./...

# Test
GOTOOLCHAIN=auto go test ./...

# Lint (must pass before PR merge)
golangci-lint run --timeout=10m

# Run locally (requires .env or env vars; DB_* + SERVICE_NAME=inventory)
go run cmd/main.go

# Apply schema migrations / dev-only demo seed
go run cmd/main.go migrate
go run cmd/main.go seed
```

## License

MIT
