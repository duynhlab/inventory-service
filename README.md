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

# Backfill stock from product-service into inventory_balances (RFC-0021 P2-2)
# Dry-run is the DEFAULT — reports and writes nothing:
go run cmd/main.go backfill
# Apply (writes balances) — explicit opt-in:
go run cmd/main.go backfill --apply       # or BACKFILL_APPLY=true
```

### `backfill` subcommand (RFC-0021 P2-2)

Migrates stock from product-service's database into `inventory_balances` so
inventory can serve reads once the phase-2 cutover flips. It reads product
**READ-ONLY** and writes inventory balances.

> **Correct ONLY at a drained cutover.** The RFC cutover runbook mandates
> draining every in-flight order (no active stock holds) **before** running the
> backfill. Do not run it against a live product database with in-flight orders.

**Mapping** — at a drained cutover it is a straight copy:

- `on_hand` = `products.stock_quantity`
- `reserved` = `0`
- `safety_stock` = `0`
- `sku_id` = product id (string); warehouse = `WH-DEFAULT`.

**Why the reservation ledger is NOT read.** Product has no commit/sold state:
`ReserveStock` decrements `stock_quantity` and inserts a `'reserved'` row, but a
*successful* order never clears that row — only the `ReleaseStock` compensation
flips it to `'released'`. So `SUM(status='reserved')` conflates in-flight holds
with **all completed sales**; adding it back would inflate `on_hand`/`reserved`
permanently with phantom reserved that can never RELEASE or COMMIT. Reading only
`products.stock_quantity` also avoids cross-table snapshot skew. Because a
drained cutover has no active holds, `reserved` is 0.

**Guards & integrity.** A negative `stock_quantity` (torn read) is reported and
the run aborts without partial writes; an apply with **zero** product rows fails
loud (almost always a misdirected `PRODUCT_DB_*` connection), never a green
no-op. Each SKU gets an opening-balance `RECEIVE` movement written in the **same
transaction** as the balance (`command_id = backfill:<run_id>:<sku>`), carrying
`on_hand_delta = on_hand`, so the append-only ledger invariant
`on_hand == SUM(on_hand_delta)` holds by construction (exactly one movement per
SKU). The whole batch is atomic — one bad row rolls back the entire run.

Flags / env:

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `--apply` | `BACKFILL_APPLY=true` | `false` (dry-run) | Write balances; omit for a report-only dry-run |
| `--dry-run` | — | `false` | Force dry-run; **overrides** `--apply`/`BACKFILL_APPLY` (safety brake) |
| `--run-id <id>` | `BACKFILL_RUN_ID` | timestamp | Audit id; also the movement `command_id` |
| `--timeout <dur>` | — | `0` (none) | Overall timeout, e.g. `5m`; run is also SIGINT/SIGTERM cancellable |

`--apply` **always refuses** a non-empty `inventory_balances` — there is no
overwrite flag. The backfill is a drained pre-cutover one-shot, and an absolute
re-copy cannot preserve the append-only ledger (it would append a second opening
movement while overwriting the balance). To **redo** before cutover, truncate
`inventory_balances` and the backfill movements, then re-run — at that point the
only movements are the backfill's own opening balances.

Product DB connection (READ-ONLY; credentials/grant provisioned separately in
homelab P2-3). Either set a full DSN or the parts:

- `PRODUCT_DB_DSN` — full `postgresql://…` DSN (wins when set), **or**
- `PRODUCT_DB_HOST`, `PRODUCT_DB_PORT` (default `5432`), `PRODUCT_DB_NAME`,
  `PRODUCT_DB_USER`, `PRODUCT_DB_PASSWORD`, `PRODUCT_DB_SSLMODE`.

`PRODUCT_DB_SSLMODE` defaults to **`require`** (cross-tenant credentials into
another service's DB); local dev opts out with `PRODUCT_DB_SSLMODE=disable`.
The inventory connection reuses the standard `DB_*` env. A mismatch, an empty
product read, a refused non-empty table, or a DB error exits non-zero.

## License

MIT
