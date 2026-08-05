-- Demo stock for local development (RFC-0021 P1-2): balances for the seeded
-- catalog products (sku_id = product id) in the default warehouse. on_hand
-- mirrors the numbers product-service used to seed, so a local read is consistent
-- with the catalog (all 13 demo SKUs covered — 1..13).
--
-- This is NOT a recovery path for a real environment: it is dev-only (the seed
-- subcommand refuses anything but ENV=development) and hard-coded to demo SKUs
-- 1..13. The phase-2 backfill from product that used to populate real environments
-- was retired in phase 4 with the column it copied, so a real balance now arrives
-- one way only — an explicit RECEIVE movement through the normal write path, which
-- keeps on_hand == SUM(on_hand_delta) intact. A fresh cluster therefore has NO
-- balances until someone puts them there, and checkout correctly fails closed
-- until then.
INSERT INTO inventory_balances (sku_id, warehouse_id, on_hand, reserved, safety_stock)
SELECT v.sku_id, w.id, v.on_hand, 0, 0
FROM (VALUES
    ('1', 50), ('2', 30), ('3', 25), ('4', 40), ('5', 20), ('6', 15), ('7', 35),
    ('8', 18), ('9', 28), ('10', 60), ('11', 75), ('12', 120), ('13', 38)
) AS v(sku_id, on_hand)
CROSS JOIN (SELECT id FROM warehouses WHERE code = 'WH-DEFAULT') AS w
ON CONFLICT (sku_id, warehouse_id) DO NOTHING;
