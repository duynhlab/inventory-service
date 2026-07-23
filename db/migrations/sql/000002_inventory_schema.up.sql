-- Inventory schema (RFC-0021 P1-2): warehouses, balances, reservations,
-- movement ledger. Derived quantities are never stored:
-- available_to_promise = GREATEST(0, on_hand - reserved - safety_stock) is
-- computed by queries, so it cannot drift from its inputs.

CREATE TABLE IF NOT EXISTS warehouses (
    id BIGSERIAL PRIMARY KEY,
    code VARCHAR(64) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'inactive')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One balance row per (sku, warehouse). sku_id is the opaque string identity
-- from inventory.v1 (initially the product id). The CHECKs are the oversell
-- backstop: correctness never relies on application-side locks alone.
CREATE TABLE IF NOT EXISTS inventory_balances (
    sku_id VARCHAR(64) NOT NULL,
    warehouse_id BIGINT NOT NULL REFERENCES warehouses(id),
    on_hand BIGINT NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    reserved BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    safety_stock BIGINT NOT NULL DEFAULT 0 CHECK (safety_stock >= 0),
    version BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sku_id, warehouse_id),
    CHECK (reserved <= on_hand)
);

-- Reservation header. id is the caller's reservation_id (= order id in the
-- initial integration); external_reference carries the order id explicitly so
-- the reconciler can classify without parsing ids. request_hash is the
-- canonical hash idempotent replays are checked against. expires_at is
-- observability-only in v1 — no sweeper releases active reservations.
CREATE TABLE IF NOT EXISTS inventory_reservations (
    id VARCHAR(255) PRIMARY KEY,
    external_reference VARCHAR(255) NOT NULL UNIQUE,
    request_hash VARCHAR(128) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'reserved'
        CHECK (status IN ('reserved', 'committed', 'released', 'expired')),
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inventory_reservations_status_created
    ON inventory_reservations(status, created_at);

CREATE TABLE IF NOT EXISTS inventory_reservation_lines (
    reservation_id VARCHAR(255) NOT NULL
        REFERENCES inventory_reservations(id) ON DELETE CASCADE,
    sku_id VARCHAR(64) NOT NULL,
    warehouse_id BIGINT NOT NULL REFERENCES warehouses(id),
    quantity BIGINT NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (reservation_id, sku_id)
);

-- Append-only movement ledger. Physical and reserved changes are separate
-- columns so RESERVE (+reserved), RELEASE (-reserved), COMMIT (-both) and
-- RECEIVE (+on_hand) audit unambiguously. command_id is the admin/command
-- idempotency key; reservation-driven movements use reference_type/id.
CREATE TABLE IF NOT EXISTS inventory_movements (
    id BIGSERIAL PRIMARY KEY,
    command_id VARCHAR(255) NOT NULL UNIQUE,
    sku_id VARCHAR(64) NOT NULL,
    warehouse_id BIGINT NOT NULL REFERENCES warehouses(id),
    type VARCHAR(32) NOT NULL
        CHECK (type IN ('RECEIVE', 'ADJUST', 'SET_SAFETY_STOCK', 'RESERVE',
                        'RELEASE', 'SALE_COMMITTED', 'RETURN')),
    on_hand_delta BIGINT NOT NULL DEFAULT 0,
    reserved_delta BIGINT NOT NULL DEFAULT 0,
    reference_type VARCHAR(32),
    reference_id VARCHAR(255),
    reason VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inventory_movements_sku_wh
    ON inventory_movements(sku_id, warehouse_id);
CREATE INDEX IF NOT EXISTS idx_inventory_movements_reference
    ON inventory_movements(reference_type, reference_id);

-- The default warehouse is infrastructure, not demo data: every environment
-- needs exactly one warehouse for the v1 one-order-one-warehouse policy, so
-- it lives in the migration (idempotent), not the seed.
INSERT INTO warehouses (code, name, status)
VALUES ('WH-DEFAULT', 'Default warehouse', 'active')
ON CONFLICT (code) DO NOTHING;
