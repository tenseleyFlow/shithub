-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP26a: organization-scoped custom secret patterns. Pattern rows are
-- metadata only: they define RE2-compatible expressions to run through
-- the existing redacting secret scanner. Matched secret bytes still
-- never leave internal/secretscan or land in storage.

-- +goose Up
CREATE TABLE secret_scan_custom_patterns (
    id            bigserial   PRIMARY KEY,
    org_id        bigint      NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name          text        NOT NULL,
    description   text        NOT NULL DEFAULT '',
    pattern       text        NOT NULL,
    min_match_len int         NOT NULL DEFAULT 8,
    enabled       boolean     NOT NULL DEFAULT true,
    created_by    bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    updated_by    bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT secret_scan_custom_patterns_name_shape CHECK (
        char_length(name) BETWEEN 1 AND 60
    ),
    CONSTRAINT secret_scan_custom_patterns_description_shape CHECK (
        char_length(description) BETWEEN 0 AND 500
    ),
    CONSTRAINT secret_scan_custom_patterns_pattern_shape CHECK (
        char_length(pattern) BETWEEN 1 AND 1000
    ),
    CONSTRAINT secret_scan_custom_patterns_min_match_len_range CHECK (
        min_match_len BETWEEN 8 AND 256
    )
);

CREATE UNIQUE INDEX secret_scan_custom_patterns_org_name_unique_idx
    ON secret_scan_custom_patterns (org_id, lower(name));

CREATE INDEX secret_scan_custom_patterns_org_enabled_idx
    ON secret_scan_custom_patterns (org_id, enabled, lower(name));

CREATE TRIGGER secret_scan_custom_patterns_set_updated_at
    BEFORE UPDATE ON secret_scan_custom_patterns
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS secret_scan_custom_patterns;
