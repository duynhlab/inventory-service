# inventory-service

The platform's **sole authority for stock**: per-warehouse balances,
all-or-nothing reservations against a derived available-to-promise, and an
append-only movement ledger.

## Responsibilities

- **Owns:** stock balances, reservations and their lifecycle, and every physical
  or reserved change as a ledger movement.
- **Does not own:** the product catalog or prices (`product-service`), order
  status (`order-service`), or the decision to buy (`checkout-service`).

Since RFC-0021 phase 4 there is no alternative stock surface to fall back to —
product's stock RPCs, read fields and schema are gone. That is deliberate, and it
is why callers fail closed rather than guessing.

## Tech

| Area | Technology |
|------|------------|
| Runtime | Go 1.26 |
| Transports | gRPC (the only business API) · HTTP for `/health` and `/ready` only |
| Data | PostgreSQL |
| Platform libraries | `dbx`, `grpcx`, `logger/zapx`, `migratex`, `obsx`, `proto` |

This service is the one that imports **no `httpx`** — it serves no business HTTP,
so it needs no shared HTTP envelope.

## API

- **Canonical contract:** [`homelab/docs/api/inventory.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/inventory.md)
- **Shared conventions:** [`homelab/docs/api/api.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/api.md)
- **Surfaces:** `inventory.v1.InventoryService` on `:9090`, called east-west by the
  order saga, checkout, and product's `/details`. No Kong route and no public
  edge — the service is NetworkPolicy-fenced. HTTP `:8080` carries only the
  probes.

Routes, RPC semantics, payloads and error reasons live in the contract. They are
not repeated here, so there is one place to change when they change.

## Run locally

Prefer the homelab **local-stack** for anything cross-service — inventory is only
interesting when a caller is reserving against it.

Standalone, you need PostgreSQL reachable via the `DB_*` variables plus
`SERVICE_NAME=inventory`:

```bash
go run cmd/main.go migrate   # apply schema migrations
go run cmd/main.go seed      # demo data — development only, refuses anything else
go run cmd/main.go           # serve gRPC :9090 + probes :8080
```

`backfill` is retired and now exits with an explanatory error. See
[AGENTS.md](./AGENTS.md) for why the arm still exists.

## Verify

The commands CI runs, so a green local run means a green pipeline:

```bash
go build ./...
go test -race ./...
go test -tags=integration ./internal/core/repository/...   # needs Docker (testcontainers)
golangci-lint run
```

## Docs

- [Canonical contract](https://github.com/duynhlab/homelab/blob/main/docs/api/inventory.md)
- [local-stack guide](https://github.com/duynhlab/homelab/blob/main/local-stack/README.md)
- [RFC-0021](https://github.com/duynhlab/homelab/tree/main/docs/proposals/rfc/RFC-0021) — why inventory owns stock, and the phased cutover that retired the product-owned surface

## License

MIT
