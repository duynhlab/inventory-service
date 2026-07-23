package v1

import (
	"context"
	"errors"
	"fmt"

	"github.com/duynhlab/inventory-service/internal/core/domain"
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

// NewReservationService creates the reservation logic service.
func NewReservationService(repo ReservationStore) *ReservationService {
	return &ReservationService{repo: repo}
}

// Reserve places an all-or-nothing hold for req. Duplicate SKU lines are
// aggregated (summed) BEFORE the repository hashes and allocates, so the
// server-side canonical hash matches the transport's drift check and the
// per-line balance math sees total demand.
func (s *ReservationService) Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	req.Items = domain.AggregateLines(req.Items)
	res, err := s.repo.Reserve(ctx, req)
	if err != nil {
		recordReservationFailure(ctx, opReserve, err)
		return domain.ReservationResult{}, fmt.Errorf("reserve: %w", err)
	}
	outcome := outcomeOK
	if res.Replayed {
		outcome = outcomeReplayed
	}
	recordReservation(ctx, opReserve, outcome)
	return res, nil
}

// Release returns a reservation's stock (saga compensation). Idempotent
// no-op successes (already released, never existed) count as ok.
func (s *ReservationService) Release(ctx context.Context, id, reason string) (string, error) {
	status, err := s.repo.Release(ctx, id, reason)
	if err != nil {
		recordReservationFailure(ctx, opRelease, err)
		return "", fmt.Errorf("release: %w", err)
	}
	recordReservation(ctx, opRelease, outcomeOK)
	return status, nil
}

// Commit converts a reservation into a sale. Committing a committed
// reservation is an idempotent replay and counts as ok.
func (s *ReservationService) Commit(ctx context.Context, id string) (string, error) {
	status, err := s.repo.Commit(ctx, id)
	if err != nil {
		recordReservationFailure(ctx, opCommit, err)
		return "", fmt.Errorf("commit: %w", err)
	}
	recordReservation(ctx, opCommit, outcomeOK)
	return status, nil
}

// GetReservation returns a reservation and its lines. Read-only and not
// metered: operation labels stay bounded to the three write commands.
func (s *ReservationService) GetReservation(ctx context.Context, id string) (domain.Reservation, error) {
	res, err := s.repo.GetReservation(ctx, id)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("get reservation: %w", err)
	}
	return res, nil
}

// recordReservationFailure counts a failed command by its business outcome.
// A canceled request is the caller hanging up, not a command outcome —
// counting it would let client churn masquerade as trouble on the on-call
// dashboard (mirrors CheckAvailability).
func recordReservationFailure(ctx context.Context, operation string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	recordReservation(ctx, operation, failureOutcome(err))
}

// failureOutcome maps a domain error onto its bounded outcome label.
func failureOutcome(err error) string {
	switch {
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
