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
