-- Movement actor (RFC-0021 P1-5): the principal that issued an admin command.
-- Admin commands write Actor here (previously smuggled through reference_id);
-- reservation-driven movements leave it NULL — their originator is the
-- workflow already identified by reference_type/reference_id.
ALTER TABLE inventory_movements ADD COLUMN IF NOT EXISTS actor VARCHAR(64);

-- Backfill: pre-migration admin rows kept the actor in reference_id. Without
-- this, replaying such a command across the deploy would compare the new
-- actor column ('' via COALESCE) against the command's Actor and
-- false-conflict. reference_id stays as written — the ledger is append-only
-- in spirit; actor is simply the authoritative column from now on.
UPDATE inventory_movements
SET actor = reference_id
WHERE reference_type = 'admin' AND actor IS NULL;
