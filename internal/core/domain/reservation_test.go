package domain_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

func TestCanonicalHash(t *testing.T) {
	base := domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 2}, {SKUID: "sku-b", Quantity: 1}}, "eu-west")

	t.Run("order-insensitive", func(t *testing.T) {
		swapped := domain.CanonicalHash([]domain.Line{{SKUID: "sku-b", Quantity: 1}, {SKUID: "sku-a", Quantity: 2}}, "eu-west")
		if swapped != base {
			t.Errorf("hash differs for reordered lines: %q vs %q", swapped, base)
		}
	})

	t.Run("quantity-sensitive", func(t *testing.T) {
		if got := domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 3}, {SKUID: "sku-b", Quantity: 1}}, "eu-west"); got == base {
			t.Error("hash unchanged for a different quantity")
		}
	})

	t.Run("destination-sensitive", func(t *testing.T) {
		if got := domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 2}, {SKUID: "sku-b", Quantity: 1}}, "us-east"); got == base {
			t.Error("hash unchanged for a different destination")
		}
	})

	t.Run("sku-set-sensitive", func(t *testing.T) {
		if got := domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 2}}, "eu-west"); got == base {
			t.Error("hash unchanged for a different sku set")
		}
	})

	t.Run("deterministic sha256 hex", func(t *testing.T) {
		if len(base) != 64 {
			t.Errorf("hash length = %d, want 64 hex chars", len(base))
		}
		if again := domain.CanonicalHash([]domain.Line{{SKUID: "sku-a", Quantity: 2}, {SKUID: "sku-b", Quantity: 1}}, "eu-west"); again != base {
			t.Errorf("hash not deterministic: %q vs %q", again, base)
		}
	})

	t.Run("does not mutate its input", func(t *testing.T) {
		in := []domain.Line{{SKUID: "sku-b", Quantity: 1}, {SKUID: "sku-a", Quantity: 2}}
		domain.CanonicalHash(in, "eu-west")
		if !reflect.DeepEqual(in, []domain.Line{{SKUID: "sku-b", Quantity: 1}, {SKUID: "sku-a", Quantity: 2}}) {
			t.Errorf("input mutated: %+v", in)
		}
	})
}

func TestAggregateLines(t *testing.T) {
	got := domain.AggregateLines([]domain.Line{
		{SKUID: "sku-a", Quantity: 2}, {SKUID: "sku-b", Quantity: 1}, {SKUID: "sku-a", Quantity: 3}})
	want := []domain.Line{{SKUID: "sku-a", Quantity: 5}, {SKUID: "sku-b", Quantity: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AggregateLines = %+v, want %+v", got, want)
	}

	// Aggregated-then-hashed equals pre-summed hash: the property the
	// transport drift check relies on.
	if domain.CanonicalHash(got, "") != domain.CanonicalHash([]domain.Line{{SKUID: "sku-b", Quantity: 1}, {SKUID: "sku-a", Quantity: 5}}, "") {
		t.Error("aggregated hash differs from pre-summed hash")
	}
}

func TestInsufficientStockErrorUnwrapsToSentinel(t *testing.T) {
	err := &domain.InsufficientStockError{
		Shortages: []domain.Shortage{{SKUID: "sku-a", Requested: 5, ATP: 1}}}
	if !errors.Is(err, domain.ErrInsufficientStock) {
		t.Error("InsufficientStockError does not match ErrInsufficientStock")
	}
	if err.Error() == "" {
		t.Error("empty error message")
	}
}
