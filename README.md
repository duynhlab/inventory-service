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

### The `backfill` subcommand was retired (RFC-0021 phase 4)

Phase 2 shipped a one-shot `backfill` subcommand that copied
`products.stock_quantity` into `inventory_balances` at the drained write cutover.
**Phase 4 removed it**, because phase 4 removed both things it depended on:

- product migration `000006` **drops** `products.stock_quantity` — the column had
  been frozen since the write cutover, so it was a stale snapshot, not a source of
  truth;
- the cross-service read-only grant that let inventory reach the product database
  (product migration `000005`, plus the `pg_hba` entry in homelab) is **revoked**.

Keeping the subcommand would have left a tool that cannot connect, reading a column
that does not exist — and a `PRODUCT_DB_*` credential surface in this service's
config for no remaining purpose. Its output survives where it matters: the opening
`RECEIVE` movements it wrote are still in `inventory_movements`, so the ledger
records where today's balances came from.

**Recovering a missing balance now** is inventory-local, and deliberately so:

1. Dev/demo → `go run cmd/main.go seed` (seeds `inventory_balances`).
2. Real correction → an explicit `RECEIVE` movement through the normal write path,
   which keeps the append-only invariant `on_hand == SUM(on_hand_delta)`.

Never reconstruct balances from product: since the write cutover, product's numbers
have not moved, so copying them back would overwrite live stock with a snapshot of
whatever was true on cutover day.

## License

MIT
