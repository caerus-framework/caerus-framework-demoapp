-- Caerus Motors (demo) — schema for lots, vehicles, and asking prices.
-- Why separate tables? Lots are "where", vehicles are "what", prices change
-- often and are what we cache in Valkey. Keep them unbundled so the demo can
-- show cache-aside on one hot column without rewriting whole car rows.

CREATE TABLE lots (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL UNIQUE,
    address    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE vehicles (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    vin        TEXT NOT NULL UNIQUE, -- demo VINs like DEMO-VIN-001 (not real ISO VINs)
    make       TEXT NOT NULL,
    model      TEXT NOT NULL,
    year       INT  NOT NULL,
    color      TEXT NOT NULL DEFAULT '',
    lot_id     UUID REFERENCES lots (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE prices (
    vehicle_id   UUID PRIMARY KEY REFERENCES vehicles (id) ON DELETE CASCADE,
    amount_cents BIGINT NOT NULL CHECK (amount_cents >= 0),
    currency     TEXT NOT NULL DEFAULT 'USD',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX vehicles_lot_id_idx ON vehicles (lot_id);
