package database

import (
	"context"

	"github.com/duynhlab/inventory-service/config"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/duynhlab/pkg/dbx"
)

// Connect builds the service's Postgres pool via the shared dbx helper. dbx
// wires otelpgx query tracing (bounded span names, no bind-parameter or
// connection PII) and pgxpool.* pool-stat metrics, and applies the
// transaction-mode-pooler-safe settings (simple protocol, statement/description
// caches off) required by the PgDog/PgBouncer pooler.
//
// The DSN is cfg.Database.BuildDSN() — the single source shared with the
// `migrate` subcommand, so the app and migrations connect identically. The
// app pool additionally sets money-path session timeouts (as pgx
// RuntimeParams via DSN query params): a reservation transaction blocked
// behind a busy balance row must fail fast into the retryable path —
// lock_timeout raises 55P03 (mapped to CONCURRENCY_CONFLICT) and
// statement_timeout raises 57014 (failed closed as DEPENDENCY_UNAVAILABLE) —
// instead of piling connections up behind a stuck lock until the pool
// starves. Migrations use the bare DSN and keep unlimited time.
func Connect(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	dsn := cfg.Database.BuildDSN() + "&lock_timeout=3s&statement_timeout=10s"
	return dbx.NewPool(ctx, dsn, dbx.WithMaxConns(cfg.Database.MaxConnections))
}
