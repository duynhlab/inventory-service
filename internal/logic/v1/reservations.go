package v1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/duynhlab/inventory-service/internal/core/domain"
	"github.com/duynhlab/pkg/logger/slogx"
)

// ReservationStore is the repository dependency of ReservationService.
// *repository.ReservationRepository satisfies it.
type ReservationStore interface {
	Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error)
	Release(ctx context.Context, id, reason string) (string, error)
	Commit(ctx context.Context, id string) (string, error)
	GetReservation(ctx context.Context, id string) (domain.Reservation, error)
}

// ReservationService implements the four write RPCs of inventory.v1
// (RFC-0021 P1-5). It is thin orchestration: aggregation + metrics here, the
// FSM and balance invariants in the repository transactions.
type ReservationService struct {
	repo ReservationStore
}

// NewReservationService creates the reservation logic service. Diagnostics go
// through the logging facade carried by the call's context.
func NewReservationService(repo ReservationStore) *ReservationService {
	return &ReservationService{repo: repo}
}

// Reserve places an all-or-nothing hold for req. Duplicate SKU lines are
// aggregated (summed) BEFORE the repository hashes and allocates, so the
// server-side canonical hash matches the transport's drift check and the
// per-line balance math sees total demand.
func (s *ReservationService) Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	ctx, span := startLogicSpan(ctx, opReserve)
	defer span.End()

	req.Items = domain.AggregateLines(req.Items)
	res, err := s.repo.Reserve(ctx, req)
	if err != nil {
		s.finishFailure(ctx, opReserve, err)
		logRejected(ctx, req.ID, failureOutcome(err))
		return domain.ReservationResult{}, fmt.Errorf("reserve: %w", err)
	}
	outcome := outcomeOK
	if res.Replayed {
		outcome = outcomeReplayed
	}
	s.finishOK(ctx, opReserve, outcome)
	return res, nil
}

// Release returns a reservation's stock (saga compensation). Idempotent
// no-op successes (already released, never existed) count as ok.
func (s *ReservationService) Release(ctx context.Context, id, reason string) (string, error) {
	ctx, span := startLogicSpan(ctx, opRelease)
	defer span.End()

	status, err := s.repo.Release(ctx, id, reason)
	if err != nil {
		s.finishFailure(ctx, opRelease, err)
		return "", fmt.Errorf("release: %w", err)
	}
	s.finishOK(ctx, opRelease, outcomeOK)
	return status, nil
}

// Commit converts a reservation into a sale. Committing a committed
// reservation is an idempotent replay and counts as ok.
func (s *ReservationService) Commit(ctx context.Context, id string) (string, error) {
	ctx, span := startLogicSpan(ctx, opCommit)
	defer span.End()

	status, err := s.repo.Commit(ctx, id)
	if err != nil {
		s.finishFailure(ctx, opCommit, err)
		return "", fmt.Errorf("commit: %w", err)
	}
	s.finishOK(ctx, opCommit, outcomeOK)
	return status, nil
}

// GetReservation returns a reservation and its lines. Read-only and not
// metered: operation labels stay bounded to the three write commands. It still
// opens a logic span so a lookup joins the trace under its transport span.
func (s *ReservationService) GetReservation(ctx context.Context, id string) (domain.Reservation, error) {
	ctx, span := startLogicSpan(ctx, opGetReservation)
	defer span.End()

	res, err := s.repo.GetReservation(ctx, id)
	if err != nil {
		// A canceled read is the caller hanging up, not our outcome — no
		// stamp, no span error (mirrors finishFailure/recordSpanError).
		if !errors.Is(err, context.Canceled) {
			outcome := failureOutcome(err)
			setSpanOutcome(ctx, outcome)
			// not_found is a business outcome, not a span error; only
			// infra/unexpected failures record the bounded span error.
			if outcome == outcomeError {
				recordBoundedSpanError(ctx, opGetReservation)
			}
		}
		return domain.Reservation{}, fmt.Errorf("get reservation: %w", err)
	}
	setSpanOutcome(ctx, outcomeOK)
	return res, nil
}

// finishOK records a successful reservation command onto its metric, span, and
// debug log with one bounded outcome (ok/replayed).
func (s *ReservationService) finishOK(ctx context.Context, operation, outcome string) {
	recordReservation(ctx, operation, outcome)
	setSpanOutcome(ctx, outcome)
	logOutcome(ctx, operation, outcome)
}

// finishFailure records a failed reservation command onto its metric, span, and
// debug log with one bounded outcome. A canceled request is the caller hanging
// up, not a command outcome — counting or error-stamping it would let client
// churn masquerade as trouble on the on-call dashboard (mirrors
// CheckAvailability). Only infra/unexpected failures are span errors; business
// rejections (insufficient/conflict/concurrency/invalid_transition/not_found)
// are normal outcomes stamped as the outcome attribute.
func (s *ReservationService) finishFailure(ctx context.Context, operation string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	outcome := failureOutcome(err)
	recordReservation(ctx, operation, outcome)
	setSpanOutcome(ctx, outcome)
	// Only infra/unexpected failures are span errors, and only a BOUNDED
	// operation-derived message is recorded — never the raw error (it carries
	// ids + SQLSTATE). Business rejections are normal outcomes.
	if outcome == outcomeError {
		recordBoundedSpanError(ctx, operation)
	}
	logOutcome(ctx, operation, outcome)
}

// logOutcome writes the debug-level diagnostic trail for one reservation
// command. A business "no" (insufficient stock, conflict) is a normal outcome,
// not an operator error, so it stays at debug. Fields are bounded to operation
// and outcome — never sku/order/reservation ids or any PII.
func logOutcome(ctx context.Context, operation, outcome string) {
	slogx.FromContext(ctx).Debug(ctx, "reservation outcome",
		slog.String(attrOperation, operation),
		slog.String(attrOutcome, outcome))
}

// logRejected emits inventory.reservation.rejected (RFC-0031 event catalog)
// when a reserve is refused for stock: insufficient, or a SKU inventory does
// not track. Other failures (conflicts, infrastructure) are not rejections of
// the request's content and write no event. The reference is the caller's
// reservation id, a correlation-only field.
func logRejected(ctx context.Context, ref, outcome string) {
	if outcome != outcomeInsufficient && outcome != outcomeUnknownSKU {
		return
	}
	slogx.FromContext(ctx).Event(ctx, slog.LevelInfo, "inventory.reservation.rejected", "reservation rejected",
		slog.String("inventory.reservation.ref", ref),
		slog.String(attrOutcome, outcome))
}

// failureOutcome maps a domain error onto its bounded outcome label.
func failureOutcome(err error) string {
	switch {
	case errors.Is(err, domain.ErrUnknownSKU):
		// A data gap, not a stockout — the same label the availability read
		// uses, so one dashboard row answers "is anything unseeded?" for both
		// paths.
		return outcomeUnknownSKU
	case errors.Is(err, domain.ErrInsufficientStock):
		return outcomeInsufficient
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return outcomeConflict
	case errors.Is(err, domain.ErrConcurrencyConflict):
		return outcomeConcurrency
	case errors.Is(err, domain.ErrInvalidTransition):
		return outcomeInvalidTransition
	case errors.Is(err, domain.ErrReservationNotFound):
		return outcomeNotFound
	default:
		return outcomeError
	}
}
