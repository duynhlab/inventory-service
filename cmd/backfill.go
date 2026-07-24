package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/duynhlab/inventory-service/config"
	database "github.com/duynhlab/inventory-service/internal/core"
	"github.com/duynhlab/inventory-service/internal/core/domain"
	"github.com/duynhlab/inventory-service/internal/core/repository"
)

// runBackfill wires the phase-2 stock backfill (RFC-0021 P2-2): a READ-ONLY
// product-service connection feeds domain.RunBackfill, which copies
// products.stock_quantity into inventory_balances (on_hand = stock_quantity,
// reserved = 0, safety_stock = 0 — correct only at a drained cutover; see the
// domain package notes) and guards every SKU before writing.
//
// Dry-run is the DEFAULT — an accidental invocation writes nothing. --apply (or
// BACKFILL_APPLY=true) writes; --dry-run OVERRIDES both as a safety brake. An
// already-populated inventory_balances is always refused (no overwrite flag);
// to redo before cutover, truncate inventory_balances + the backfill movements
// and re-run. The context is cancellable via SIGINT/SIGTERM and an optional
// --timeout. It returns an error (→ non-zero exit) on a mismatch, an empty
// product read, a refused non-empty table, or a DB failure.
func runBackfill(cfg *config.Config, logger *zap.Logger, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	apply := fs.Bool("apply", envBool("BACKFILL_APPLY"),
		"write balances (default: dry-run, report only)")
	dryRun := fs.Bool("dry-run", false, "force dry-run; overrides --apply and BACKFILL_APPLY")
	runID := fs.String("run-id", "", "audit run id (default: BACKFILL_RUN_ID or a timestamp)")
	timeout := fs.Duration("timeout", 0, "overall timeout, e.g. 5m (0 = no timeout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" {
		*runID = firstNonEmpty(os.Getenv("BACKFILL_RUN_ID"), "backfill-"+time.Now().UTC().Format(time.RFC3339))
	}
	// --dry-run is the safety brake: it wins over --apply / BACKFILL_APPLY.
	effectiveApply := *apply && !*dryRun

	// Cancellable on Ctrl-C / SIGTERM so a long run stops cleanly; the optional
	// timeout bounds the whole operation.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	productPool, err := connectProductDB(ctx)
	if err != nil {
		return err
	}
	defer productPool.Close()

	invPool, err := database.Connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect inventory DB: %w", err)
	}
	defer invPool.Close()

	reader := &productDBReader{pool: productPool}
	writer := repository.NewBackfillRepository(invPool)

	report, runErr := domain.RunBackfill(ctx, reader, writer,
		domain.BackfillOptions{RunID: *runID, Apply: effectiveApply})

	logReport(logger, report, effectiveApply)
	return runErr
}

// connectProductDB opens the READ-ONLY product pool. Simple query protocol is
// set for transaction-pooler safety (product may sit behind PgBouncer/PgDog),
// matching the seed path; the read-only guard itself is enforced per query via
// a read-only transaction in productDBReader.
func connectProductDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn, err := config.ProductDBDSN()
	if err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse product DSN: %w", err)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect product DB: %w", err)
	}
	return pool, nil
}

// productDBReader reads product-service stock READ-ONLY (domain.ProductReader).
// Only products.stock_quantity is read; the reservation ledger is deliberately
// left untouched (see the domain package notes). The read runs inside a
// read-only transaction so the backfill can never mutate the authoritative
// product database.
type productDBReader struct {
	pool *pgxpool.Pool
}

func (r *productDBReader) Products(ctx context.Context) ([]domain.ProductRow, error) {
	// The caller (domain.RunBackfill) adds the "read products" context, so
	// readInTx errors are returned as-is to avoid a redundant double prefix.
	var out []domain.ProductRow
	err := r.readInTx(ctx, `SELECT id::text, stock_quantity FROM products ORDER BY id`,
		func(rows pgx.Rows) error {
			var p domain.ProductRow
			if err := rows.Scan(&p.ProductID, &p.StockQuantity); err != nil {
				return fmt.Errorf("scan product: %w", err)
			}
			out = append(out, p)
			return nil
		})
	return out, err
}

// readInTx runs q inside a READ ONLY transaction against product and applies
// scan to every row. The read-only access mode is the guard that the backfill
// never writes to product.
func (r *productDBReader) readInTx(ctx context.Context, q string, scan func(pgx.Rows) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin read-only: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback of a read-only tx is a no-op

	rows, err := tx.Query(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func logReport(logger *zap.Logger, report domain.BackfillReport, apply bool) {
	logger.Info("Backfill report",
		zap.String("run_id", report.RunID),
		zap.Bool("apply", apply),
		zap.String("result", report.Result),
		zap.Int("product_count", report.ProductCount),
		zap.Int("target_count", report.TargetCount),
		zap.Int("mismatch_count", report.MismatchCount),
		zap.Int("changed", report.Changed),
		zap.Int64("warehouse_id", report.WarehouseID),
		zap.Bool("applied", report.Applied),
	)
	for _, m := range report.Mismatches {
		logger.Warn("Backfill mismatch",
			zap.String("run_id", report.RunID),
			zap.String("sku_id", m.SKUID),
			zap.String("reason", m.Reason),
			zap.Int64("on_hand", m.OnHand),
		)
	}
}

// envBool reports whether an env var is a truthy "true"/"1"/"yes".
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
