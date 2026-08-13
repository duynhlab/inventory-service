package v1

import (
	"context"
	"errors"
	"testing"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

// fakeCommander scripts the StockCommander port.
type fakeCommander struct {
	applied bool
	err     error
	got     domain.StockCommand
	calls   int
}

func (f *fakeCommander) ReceiveStock(_ context.Context, cmd domain.StockCommand) (bool, error) {
	f.got, f.calls = cmd, f.calls+1
	return f.applied, f.err
}

func (f *fakeCommander) AdjustOnHand(_ context.Context, cmd domain.StockCommand) (bool, error) {
	f.got, f.calls = cmd, f.calls+1
	return f.applied, f.err
}

func (f *fakeCommander) SetSafetyStock(_ context.Context, cmd domain.StockCommand) (bool, error) {
	f.got, f.calls = cmd, f.calls+1
	return f.applied, f.err
}

func validCmd() domain.StockCommand {
	return domain.StockCommand{
		CommandID:   "cmd-1",
		SKUID:       "SKU-1",
		WarehouseID: 1,
		Quantity:    5,
		Actor:       "a11ce000-0000-4000-8000-000000000001",
	}
}

func TestAdminCommandValidationFailsBeforeRepository(t *testing.T) {
	cmd := validCmd()
	cmd.CommandID = "" // would collide idempotency keys — must never reach the repo

	commander := &fakeCommander{}
	svc := NewAdminService(nil, commander)

	_, err := svc.ReceiveStock(context.Background(), cmd)
	if !errors.Is(err, domain.ErrInvalidCommand) {
		t.Fatalf("want ErrInvalidCommand, got %v", err)
	}
	if commander.calls != 0 {
		t.Fatalf("repository must not be called for an invalid command")
	}
}

func TestAdminCommandOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		applied     bool
		err         error
		wantApplied bool
		wantErr     bool
	}{
		{name: "applied", applied: true, wantApplied: true},
		{name: "idempotent replay", applied: false, wantApplied: false},
		{name: "repository refusal", err: domain.ErrInsufficientOnHand, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewAdminService(nil, &fakeCommander{applied: tc.applied, err: tc.err})
			applied, err := svc.AdjustOnHand(context.Background(), validCmd())
			if tc.wantErr {
				if !errors.Is(err, domain.ErrInsufficientOnHand) {
					t.Fatalf("want repository sentinel preserved, got %v", err)
				}
				return
			}
			if err != nil || applied != tc.wantApplied {
				t.Fatalf("want applied=%v err=nil, got applied=%v err=%v", tc.wantApplied, applied, err)
			}
		})
	}
}

// fakeReader scripts the AdminReader port.
type fakeReader struct {
	balances     []domain.BalanceView
	movements    []domain.MovementView
	reservations []domain.ReservationView
	total        int
	err          error
}

func (f *fakeReader) ListBalances(_ context.Context, _ domain.BalanceFilter, _, _ int) ([]domain.BalanceView, int, error) {
	return f.balances, f.total, f.err
}

func (f *fakeReader) SKUBalances(_ context.Context, _ string) ([]domain.BalanceView, error) {
	return f.balances, f.err
}

func (f *fakeReader) ListMovements(_ context.Context, _ domain.MovementFilter, _, _ int) ([]domain.MovementView, int, error) {
	return f.movements, f.total, f.err
}

func (f *fakeReader) ListReservations(_ context.Context, _ string, _, _ int) ([]domain.ReservationView, int, error) {
	return f.reservations, f.total, f.err
}

func TestAdminReadsPassThrough(t *testing.T) {
	reader := &fakeReader{
		balances:     []domain.BalanceView{{SKUID: "S", ATP: 3}},
		movements:    []domain.MovementView{{ID: 1, Type: "RECEIVE"}},
		reservations: []domain.ReservationView{{ID: "r1", Status: "reserved"}},
		total:        7,
	}
	svc := NewAdminService(reader, nil)
	ctx := context.Background()

	if items, total, err := svc.ListBalances(ctx, domain.BalanceFilter{}, 20, 0); err != nil || total != 7 || len(items) != 1 {
		t.Fatalf("ListBalances = (%d, %d, %v)", len(items), total, err)
	}
	if items, err := svc.SKUBalances(ctx, "S"); err != nil || len(items) != 1 {
		t.Fatalf("SKUBalances = (%d, %v)", len(items), err)
	}
	if items, total, err := svc.ListMovements(ctx, domain.MovementFilter{}, 20, 0); err != nil || total != 7 || len(items) != 1 {
		t.Fatalf("ListMovements = (%d, %d, %v)", len(items), total, err)
	}
	if items, total, err := svc.ListReservations(ctx, "", 20, 0); err != nil || total != 7 || len(items) != 1 {
		t.Fatalf("ListReservations = (%d, %d, %v)", len(items), total, err)
	}
}

func TestAdminReadsWrapErrors(t *testing.T) {
	sentinel := errors.New("pg down")
	svc := NewAdminService(&fakeReader{err: sentinel}, nil)
	ctx := context.Background()

	if _, _, err := svc.ListBalances(ctx, domain.BalanceFilter{}, 20, 0); !errors.Is(err, sentinel) {
		t.Fatalf("ListBalances error not preserved: %v", err)
	}
	if _, err := svc.SKUBalances(ctx, "S"); !errors.Is(err, sentinel) {
		t.Fatalf("SKUBalances error not preserved: %v", err)
	}
	if _, _, err := svc.ListMovements(ctx, domain.MovementFilter{}, 20, 0); !errors.Is(err, sentinel) {
		t.Fatalf("ListMovements error not preserved: %v", err)
	}
	if _, _, err := svc.ListReservations(ctx, "", 20, 0); !errors.Is(err, sentinel) {
		t.Fatalf("ListReservations error not preserved: %v", err)
	}
}
