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
//
// When DB_PASSWORD_FILE is set (ADR-025 pattern A), WithPasswordFile makes
// dbx read the password from that mounted file on every new connection, so a
// rotated password is used without a pod restart. The DSN password is then
// ignored: a missing or empty file fails the connection instead of falling
// back. dbx also caps connection lifetime at 30m (±3m) so the pool moves onto
// a rotated password. An empty path is a no-op (DB_PASSWORD as before).
func Connect(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	dsn := cfg.Database.BuildDSN() + "&lock_timeout=3s&statement_timeout=10s"
	return dbx.NewPool(ctx, dsn,
		dbx.WithMaxConns(cfg.Database.MaxConnections),
		dbx.WithPasswordFile(cfg.Database.PasswordFile),
	)
}
