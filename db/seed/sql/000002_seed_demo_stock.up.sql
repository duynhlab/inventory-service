-- Demo stock for local development (RFC-0021 P1-2): balances for the ten
-- seeded catalog products (sku_id = product id) in the default warehouse.
-- Real environments are populated by the phase-2 backfill from product,
-- never by this seed (the seed subcommand refuses production).
INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
SELECT s.sku_id, w.id, 100, 0, 0
FROM (VALUES ('1'), ('2'), ('3'), ('4'), ('5'), ('6'), ('7'), ('8'), ('9'), ('10')) AS s(sku_id)
CROSS JOIN (SELECT id FROM warehouses WHERE code = 'WH-DEFAULT') AS w
ON CONFLICT (sku_id, warehouse_id) DO NOTHING;
