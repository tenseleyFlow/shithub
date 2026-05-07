-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Initial migration: a generic key/value `meta` table used for
-- schema-version-friendly metadata, plus the canonical `tg_set_updated_at`
-- trigger function reused by every later table that has an `updated_at`
-- column.
--
-- This is the only migration where we bootstrap the trigger; subsequent
-- migrations attach the trigger to their tables via:
--
--     CREATE TRIGGER set_updated_at BEFORE UPDATE ON <table>
--         FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TABLE meta (
    key        text        PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON meta
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

INSERT INTO meta (key, value) VALUES
    ('schema_version', '"0001"'::jsonb),
    ('app',            '"shithub"'::jsonb);

-- +goose Down
DROP TABLE IF EXISTS meta;
DROP FUNCTION IF EXISTS tg_set_updated_at();
