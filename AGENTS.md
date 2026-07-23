# inventory-service AGENTS guide

Instructions for AI agents and human contributors working in this repository.
Read it before making changes; keep edits surgical and consistent with what is
already here.

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

## Project overview

Inventory microservice for the `duynhlab` platform — the sole authority for
stock balances, reservations, allocations, and the stock-movement ledger.
Designed in **RFC-0021** (duynhlab/homelab `docs/proposals/rfc/RFC-0021/`),
which supersedes the product-owned stock surface. Go module
`github.com/duynhlab/inventory-service`. It serves operational HTTP on `:8080`
(`/health`, `/ready`) and a gRPC server (`inventory.v1.InventoryService`) on
`:9090` — gRPC is the only business API surface.

**Status: Bootstrap skeleton (RFC-0021 P1-1).** No business logic yet: every
RPC returns `Unimplemented`, and the migration/seed files are placeholders.
The schema lands in P1-2; the RPC implementations land in P1-3..P1-5.

**API truth:** the canonical contract lives in duynhlab/homelab
`docs/api/` (`inventory.md` forthcoming). When this README and homelab
disagree, homelab wins — file a drift fix.

## Repository layout

Target shape is the platform's 3-layer architecture (dependencies flow one way
only: `grpc → logic → core`); the skeleton ships the transport and core edges.

```
inventory-service/
├── cmd/main.go                       # Wires HTTP (:8080) + gRPC (:9090), migrate/seed subcommands, graceful shutdown
├── config/config.go                  # Env-driven configuration + validation
├── db/migrations/                    # golang-migrate SQL (sql/000001_*.up.sql) + embed.go — placeholder until P1-2
├── db/seed/                          # DEV-ONLY demo seed SQL + embed.go — placeholder until P1-2
└── internal/
    ├── core/database.go              # pgx/v5 pool via pkg/dbx (pooler-safe: simple protocol)
    └── grpc/v1/server.go             # inventory.v1.InventoryService server (Unimplemented until P1-3..P1-5)
```

## Build, test, lint

```bash
GOTOOLCHAIN=auto go build ./...   # compile (go.mod pins go 1.26.2)
GOTOOLCHAIN=auto go vet ./...     # vet
GOTOOLCHAIN=auto go test ./...    # tests
golangci-lint run                 # lint — must pass
```

## Conventions

- **3-layer architecture**, dependencies flow one way only: transport → logic →
  core. Transport adapters (gRPC) handle validation/mapping and delegate; Logic
  holds business rules and calls repository interfaces (no SQL, no transport
  types); Core owns domain models, the repository interface, and the Postgres
  implementation. Core imports nothing from transport or logic.
- **gRPC SERVER**: this service exposes `inventory.v1.InventoryService` on
  `:9090` (`GRPC_PORT`). gRPC is the official east-west transport, so the
  server always runs; HTTP `:8080` carries only `/health` and `/ready`.
  Bootstrap via shared `github.com/duynhlab/pkg/grpcx` (`grpcx.NewServer`
  provides OpenTelemetry interceptors, health, reflection). Proto lives in
  `github.com/duynhlab/pkg/proto/inventory/v1`; errors carry stable reasons
  via `grpcx.ErrorWithReason` (see the proto doc comments).
- **Observability** via shared `github.com/duynhlab/pkg/obsx`:
  `obsx.SetupObservability` is the single OTel wiring point (traces, OTLP
  metrics, OTLP logs); logs tee through `obs.ZapCore` for trace/log
  correlation; Pyroscope profiling behind `PROFILING_ENABLED`.
- **Database** via shared `github.com/duynhlab/pkg/dbx` (`dbx.NewPool`):
  otelpgx tracing, pool-stat metrics, transaction-mode-pooler-safe settings.
- **Diagrams**: Mermaid only. Never ASCII art.

## Gotchas

- The gRPC server (`internal/grpc/v1/server.go`) is a transport adapter: once
  logic exists it must never touch the database directly or embed business
  rules.
- Kyverno admission rejects bad images: pin `ghcr.io/duynhlab/<service>:<sha>`
  or `:vX.Y.Z`, never `:latest`.
- Migrations run via golang-migrate, embedded through `db/migrations/embed.go`
  (`embed.FS`) and applied by `pkg/migratex` from the `migrate` subcommand. The
  init container reuses the app image (`args: ["migrate"]`) — no separate
  migration image. Migrations are forward-only `*.up.sql` files.
- `seed` is DEV-ONLY and refuses to run when `ENV` is production; never wire it
  into the `migrate` or serve paths.
