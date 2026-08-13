package v1

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// Admin operations (RFC-0023 slice A): the protected Backoffice reads and the
// first callers of the idempotent stock commands. Span names follow the
// existing logic-span convention; the command counter's labels are bounded to
// enumerable domain values (operation, outcome) — never SKU or actor ids.
const (
	opListBalances     = "admin_list_balances"
	opSKUBalances      = "admin_sku_balances"
	opListMovements    = "admin_list_movements"
	opListReservations = "admin_list_reservations"
	opReceiveStock     = "admin_receive_stock"
	opAdjustOnHand     = "admin_adjust_on_hand"
)

// Admin command outcomes (bounded).
const (
	cmdOutcomeApplied  = "applied"
	cmdOutcomeReplayed = "replayed"
	cmdOutcomeRejected = "rejected"
	cmdOutcomeError    = "error"
)

var adminCommandCounter, _ = meter.Int64Counter("inventory.admin.command.total",
	metric.WithDescription("Protected stock commands, split by operation and outcome"))

func recordAdminCommand(ctx context.Context, operation, outcome string) {
	adminCommandCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.String("outcome", outcome),
	))
}

// AdminReader is the read port of the protected views.
type AdminReader interface {
	ListBalances(ctx context.Context, f domain.BalanceFilter, limit, offset int) ([]domain.BalanceView, int, error)
	SKUBalances(ctx context.Context, skuID string) ([]domain.BalanceView, error)
	ListMovements(ctx context.Context, f domain.MovementFilter, limit, offset int) ([]domain.MovementView, int, error)
	ListReservations(ctx context.Context, status string, limit, offset int) ([]domain.ReservationView, int, error)
}

// AdminService is the logic layer of the protected Backoffice surface. Reads
// pass through with observability; commands validate the domain command and
// delegate to the transactional StockCommander (idempotency and balance
// invariants live in the repository and the schema).
type AdminService struct {
	reads    AdminReader
	commands domain.StockCommander
}

// NewAdminService creates the admin logic service.
func NewAdminService(reads AdminReader, commands domain.StockCommander) *AdminService {
	return &AdminService{reads: reads, commands: commands}
}

// ListBalances returns one balances page plus the filter's total row count.
func (s *AdminService) ListBalances(ctx context.Context, f domain.BalanceFilter, limit, offset int) ([]domain.BalanceView, int, error) {
	ctx, span := startLogicSpan(ctx, opListBalances)
	defer span.End()
	items, total, err := s.reads.ListBalances(ctx, f, limit, offset)
	if err != nil {
		recordSpanError(ctx, opListBalances, err)
		return nil, 0, fmt.Errorf("list balances: %w", err)
	}
	return items, total, nil
}

// SKUBalances returns every warehouse balance for one SKU; the empty slice
// means the SKU is untracked.
func (s *AdminService) SKUBalances(ctx context.Context, skuID string) ([]domain.BalanceView, error) {
	ctx, span := startLogicSpan(ctx, opSKUBalances)
	defer span.End()
	items, err := s.reads.SKUBalances(ctx, skuID)
	if err != nil {
		recordSpanError(ctx, opSKUBalances, err)
		return nil, fmt.Errorf("sku balances: %w", err)
	}
	return items, nil
}

// ListMovements returns one ledger page plus the filter's total row count.
func (s *AdminService) ListMovements(ctx context.Context, f domain.MovementFilter, limit, offset int) ([]domain.MovementView, int, error) {
	ctx, span := startLogicSpan(ctx, opListMovements)
	defer span.End()
	items, total, err := s.reads.ListMovements(ctx, f, limit, offset)
	if err != nil {
		recordSpanError(ctx, opListMovements, err)
		return nil, 0, fmt.Errorf("list movements: %w", err)
	}
	return items, total, nil
}

// ListReservations returns one reservation-header page plus the total.
func (s *AdminService) ListReservations(ctx context.Context, status string, limit, offset int) ([]domain.ReservationView, int, error) {
	ctx, span := startLogicSpan(ctx, opListReservations)
	defer span.End()
	items, total, err := s.reads.ListReservations(ctx, status, limit, offset)
	if err != nil {
		recordSpanError(ctx, opListReservations, err)
		return nil, 0, fmt.Errorf("list reservations: %w", err)
	}
	return items, total, nil
}

// ReceiveStock applies (or idempotently replays) a stock receipt.
func (s *AdminService) ReceiveStock(ctx context.Context, cmd domain.StockCommand) (bool, error) {
	return s.runCommand(ctx, opReceiveStock, cmd, s.commands.ReceiveStock)
}

// AdjustOnHand applies (or idempotently replays) a signed on-hand adjustment.
func (s *AdminService) AdjustOnHand(ctx context.Context, cmd domain.StockCommand) (bool, error) {
	return s.runCommand(ctx, opAdjustOnHand, cmd, s.commands.AdjustOnHand)
}

// runCommand executes one stock command with uniform tracing and metrics.
// Validation errors count as rejected; anything else the repository refuses
// (insufficient stock, database failure) counts by its error identity at the
// web layer — here it is uniformly an error outcome.
func (s *AdminService) runCommand(
	ctx context.Context,
	operation string,
	cmd domain.StockCommand,
	run func(context.Context, domain.StockCommand) (bool, error),
) (bool, error) {
	ctx, span := startLogicSpan(ctx, operation)
	defer span.End()

	if err := cmd.Validate(); err != nil {
		recordAdminCommand(ctx, operation, cmdOutcomeRejected)
		return false, fmt.Errorf("%w: %w", domain.ErrInvalidCommand, err)
	}

	applied, err := run(ctx, cmd)
	switch {
	case err != nil:
		recordSpanError(ctx, operation, err)
		recordAdminCommand(ctx, operation, cmdOutcomeError)
		return false, fmt.Errorf("%s: %w", operation, err)
	case applied:
		recordAdminCommand(ctx, operation, cmdOutcomeApplied)
	default:
		recordAdminCommand(ctx, operation, cmdOutcomeReplayed)
	}
	return applied, nil
}
