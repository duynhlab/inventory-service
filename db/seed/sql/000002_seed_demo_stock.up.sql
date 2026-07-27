-- Demo stock for local development (RFC-0021 P1-2): balances for the seeded
-- catalog products (sku_id = product id) in the default warehouse. on_hand
-- mirrors product-service's own demo stock so a local shadow/inventory read is
-- consistent with product (all 13 demo SKUs covered — 1..13). Real environments
-- are populated by the phase-2 backfill from product, never by this seed (the
-- seed subcommand refuses production).
INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
SELECT v.sku_id, w.id, v.on_hand, 0, 0
FROM (VALUES
    ('1', 50), ('2', 30), ('3', 25), ('4', 40), ('5', 20), ('6', 15), ('7', 35),
    ('8', 18), ('9', 28), ('10', 60), ('11', 75), ('12', 120), ('13', 38)
) AS v(sku_id, on_hand)
CROSS JOIN (SELECT id FROM warehouses WHERE code = 'WH-DEFAULT') AS w
ON CONFLICT (sku_id, warehouse_id) DO NOTHING;
