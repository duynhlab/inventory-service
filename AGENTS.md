# inventory-service AGENTS guide

Instructions for AI agents and human contributors working in this repository.
Read it before making changes; keep edits surgical and consistent with what is
already here.

## Authority and scope

This repository implements the service. It does **not** define the contract.

- **Canonical contract:** [`homelab/docs/api/inventory.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/inventory.md)
- **Shared API rules:** [`homelab/docs/api/api.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/api.md)

Implement against those files. When this repository and the contract disagree,
**stop and classify the mismatch** using
[Resolving a mismatch](https://github.com/duynhlab/homelab/blob/main/docs/api/README.md#resolving-a-mismatch)
before editing either side. One class — an implementation that violates the
intended contract — **blocks the release tag**; it is fixed in code or explicitly
accepted, never by rewriting the contract to match what the code happens to do.

No route, RPC, payload or error inventory belongs in this file. Manifests,
gateway routing, NetworkPolicy and platform observability belong to
[duynhlab/homelab](https://github.com/duynhlab/homelab).

## Contribution workflow

- Never commit or push to `main`. Branch first, then open a PR.
- Branch names use conventional prefixes: `feat/`, `fix/`, `docs/`, `chore/`,
  `refactor/`, `test/`.
- Commit subjects: imperative mood, capitalised, ≤ 50 characters, no trailing
  period (`Add Reserve RPC`, not `Added`/`Adds`). Add a body wrapped
  at 72 characters only when the change is non-trivial.
- Do not add attribution trailers (`Signed-off-by`, `Co-authored-by`,
  `Generated-by`, etc.), GitHub issue references, or `@`-mentions in commit
  messages. Put issue links in the PR description.
- PRs are squash-merged. CI (`go-check`) runs build, test, and lint on every PR
  and must be green before merge.
- Docs are English only. Diagrams are Mermaid only — never ASCII art.

## Code quality

- Run `golangci-lint run` (v2+, `.golangci.yml`, 60+ linters) and fix every
  finding before committing. Common fixes:
  - `perfsprint`: prefer `errors.New` over `fmt.Errorf` when there are no verbs.
  - `nosprintfhostport`: use `net.JoinHostPort` over `fmt.Sprintf("%s:%s", …)`.
  - `errcheck`: check every error return, or explicitly `_ = fn()`.
  - `noctx`: use the `*WithContext` request constructors.
  - `goconst` / `gocognit`: extract repeated literals and split complex funcs.
- Keep changes idiomatic: dependency injection via constructor parameters,
  structured logging with `zap`, context propagation on all I/O.
- Write tests for new logic (stdlib `testing`, hand-written mocks,
  table-driven subtests — no testify/gomock).
- Before pushing or opening a PR, verify Sonar new-code coverage ≥80%: run
  `go test -race -coverprofile=coverage.out ./...` and confirm changed lines are
  covered, including BOTH branches of any new conditional. `**/cmd/**`,
  `**/db/migrations/**`, `**/core/repository/**` are coverage-excluded;
  everything else counts.

## Build, test, lint

These are the commands CI runs, so a green local run means a green pipeline.

```bash
go build ./...
go vet ./...
go test -race ./...
go test -tags=integration ./internal/core/repository/...   # needs Docker (testcontainers)
golangci-lint run
```

Local development against an unreleased `pkg`: `pkg` is one module per package
since v0.36, so its root has no `go.mod` and a single
`replace github.com/duynhlab/pkg` can no longer resolve. Use one commented
`replace` line per module — the trailer in `go.mod` shows the shape, and
[`docs/api/pkg.md`](https://github.com/duynhlab/homelab/blob/main/docs/api/pkg.md)
explains why.

## Architecture boundaries

**3-layer, dependencies flow one way only: transport → logic → core.**

- **Transport** (`internal/grpc/v1/`) validates, maps, and delegates. It must
  never touch the database or hold business rules.
- **Logic** (`internal/logic/v1/`) holds the business rules and calls repository
  interfaces — no SQL, no transport types.
- **Core** (`internal/core/`) owns domain models, the repository interface, and
  the Postgres implementation. It imports nothing from transport or logic.

gRPC is the only business surface: the server always runs, and HTTP `:8080`
carries only `/health` and `/ready`. Bootstrap through
`github.com/duynhlab/pkg/grpcx` (`grpcx.NewServer` supplies OpenTelemetry
interceptors, health, reflection); errors carry stable machine-readable reasons
via `grpcx.ErrorWithReason`. Observability is wired once through
`github.com/duynhlab/pkg/obsx`; the database pool comes from
`github.com/duynhlab/pkg/dbx`, which applies the transaction-pooler-safe
settings.

## Repository map

- `cmd/main.go` — wires gRPC `:9090` + probes `:8080`, dispatches subcommands, orders graceful shutdown
- `config/config.go` — env-driven configuration and startup validation
- `db/migrations/` — forward-only golang-migrate SQL, embedded via `embed.FS`
- `db/seed/` — development-only demo seed, embedded the same way
- `internal/grpc/v1/` — the `inventory.v1.InventoryService` server and request validation
- `internal/logic/v1/` — availability and reservation rules, metrics, tracing helpers
- `internal/core/domain/` — inventory and reservation models
- `internal/core/repository/` — Postgres implementation and the integration tests
- `middleware/` — tracing for the probe listener

## Invariants

Rules an implementer can violate at the keyboard. They are not restated in the
contract's language; they are the local reasons the contract holds.

- **The ledger is append-only.** Balances are explained by their movements —
  `on_hand == SUM(on_hand_delta)`. Never adjust a balance without writing the
  movement that accounts for it.
- **Never reconstruct a balance from product.** Product's stock numbers have not
  moved since the write cutover and the column is now dropped; copying them back
  would overwrite live stock with a snapshot of cutover day. Recover a missing
  balance inventory-locally: `seed` in development, or an explicit `RECEIVE`
  movement through the normal write path, which keeps the append-only invariant
  true.
- **A retired subcommand stays as an explicit refusal.** `backfill` was retired
  in RFC-0021 phase 4, but its `case` arm remains and calls `Fatal` with the
  reason. Deleting the arm would be worse than useless: `default` falls through
  to serving, so `inventory backfill --apply` would start a full gRPC + HTTP
  server inside a one-shot Job, holding database credentials, with `--apply`
  silently discarded as an unparsed argument and nothing to reap it. A removed
  subcommand has to say it was removed.
- **Fail closed, and let the data gap win.** A storage error is never reported as
  "out of stock". In a mixed result an unknown SKU outranks a shortage, because
  the caller's fail-closed default for an unknown SKU is retryable — the safe
  direction. `CheckAvailability` is advisory; `Reserve` is the correctness gate.
- **Reservation replays are idempotent, not duplicate work.** The reservation id
  is the idempotency key: the same order id returns the committed result, an
  already-released reservation is a no-op success, and a committed one replays.
- **`seed` is development-only** and refuses to run anywhere else — staging and
  unrecognised environments are refused too, not just production. Never wire it
  into `migrate` or the serve path, and never let it share the
  `schema_migrations` version table.
- **Migrations run against the direct database host, never a transaction
  pooler** — DDL through PgBouncer/PgDog is unsafe. They are applied by
  `pkg/migratex` from the `migrate` subcommand, and the init container reuses the
  app image (`args: ["migrate"]`).

## Gotchas

- Kyverno admission rejects bad images. The published image is
  `ghcr.io/duynhlab/inventory-service/inventory-service:<tag>` — the repository
  path repeats, and there is no separate migration image. Pin a `sha` or
  `vX.Y.Z`, never `:latest`.
- The `Unimplemented` symbol in `internal/grpc/v1/server.go` is gRPC's
  forward-compatibility embed, not behaviour. Every RPC is implemented.

## API change synchronization

An API change is not done when the code compiles.

- The contract in homelab and this repository move **together** — same change,
  and either the same PR pair or an immediate follow-up.
- Behaviour that is designed but not deployed is marked **`Planned`** in the
  contract; it is never described as current.
- A material mismatch between the contract and this implementation **blocks the
  release tag** until it is reconciled or explicitly accepted.
