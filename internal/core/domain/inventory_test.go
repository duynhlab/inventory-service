package domain_test

import (
	"strings"
	"testing"

	"github.com/duynhlab/inventory-service/internal/core/domain"
)

func validCommand() domain.StockCommand {
	return domain.StockCommand{
		CommandID:   "cmd-1",
		SKUID:       "sku-1",
		WarehouseID: 1,
		Quantity:    1,
	}
}

func TestStockCommandValidate(t *testing.T) {
	if err := validCommand().Validate(); err != nil {
		t.Fatalf("valid command Validate() = %v, want nil", err)
	}

	tests := []struct {
		name    string
		mut     func(*domain.StockCommand)
		wantMsg string
	}{
		{"empty command id", func(c *domain.StockCommand) { c.CommandID = "" }, "command_id"},
		{"oversized command id", func(c *domain.StockCommand) { c.CommandID = strings.Repeat("x", 256) }, "command_id"},
		{"empty sku id", func(c *domain.StockCommand) { c.SKUID = "" }, "sku_id"},
		{"oversized sku id", func(c *domain.StockCommand) { c.SKUID = strings.Repeat("x", 65) }, "sku_id"},
		{"zero warehouse id", func(c *domain.StockCommand) { c.WarehouseID = 0 }, "warehouse_id"},
		{"negative warehouse id", func(c *domain.StockCommand) { c.WarehouseID = -1 }, "warehouse_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validCommand()
			tt.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error for %q", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Validate() = %q, want mention of %q", err, tt.wantMsg)
			}
		})
	}

	t.Run("boundary lengths pass", func(t *testing.T) {
		c := validCommand()
		c.CommandID = strings.Repeat("x", 255)
		c.SKUID = strings.Repeat("x", 64)
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() at schema max lengths = %v, want nil", err)
		}
	})
}
