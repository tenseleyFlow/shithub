-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29 — Organization platform security settings.

-- +goose Up

CREATE TABLE org_security_settings (
    org_id             bigint      PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    require_two_factor boolean     NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON org_security_settings
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS org_security_settings;
