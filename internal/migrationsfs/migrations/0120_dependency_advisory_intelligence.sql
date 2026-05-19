-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25d: advisory intelligence metadata for operator-controlled imports.
-- Runtime dependency matching remains local-only; these tables store source
-- provenance, aliases, affected ranges, and sync audit history so importers can
-- reconcile external advisory files without replacing manual advisories.

-- +goose Up
ALTER TABLE dependency_advisories
    ADD COLUMN modified_at timestamptz NULL,
    ADD COLUMN source_url text NOT NULL DEFAULT '',
    ADD COLUMN cvss_score numeric(4,1) NULL,
    ADD COLUMN cvss_vector text NOT NULL DEFAULT '',
    ADD COLUMN cwe_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT dependency_advisories_source_url_shape CHECK (char_length(source_url) <= 2048),
    ADD CONSTRAINT dependency_advisories_cvss_score_range CHECK (cvss_score IS NULL OR (cvss_score >= 0 AND cvss_score <= 10)),
    ADD CONSTRAINT dependency_advisories_cvss_vector_shape CHECK (char_length(cvss_vector) <= 255),
    ADD CONSTRAINT dependency_advisories_cwe_ids_array CHECK (jsonb_typeof(cwe_ids) = 'array');

CREATE TABLE dependency_advisory_sources (
    name             text        PRIMARY KEY,
    kind             text        NOT NULL DEFAULT 'manual',
    display_name     text        NOT NULL,
    url              text        NOT NULL DEFAULT '',
    license          text        NOT NULL DEFAULT '',
    attribution      text        NOT NULL DEFAULT '',
    enabled          boolean     NOT NULL DEFAULT true,
    last_sync_at     timestamptz NULL,
    last_sync_status text        NOT NULL DEFAULT 'never',
    last_sync_error  text        NOT NULL DEFAULT '',
    cursor_value     text        NOT NULL DEFAULT '',
    etag             text        NOT NULL DEFAULT '',
    metadata         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_advisory_sources_name_shape CHECK (char_length(name) BETWEEN 1 AND 80),
    CONSTRAINT dependency_advisory_sources_kind_known CHECK (kind IN ('manual', 'osv', 'github_advisory_database', 'file')),
    CONSTRAINT dependency_advisory_sources_display_name_shape CHECK (char_length(display_name) BETWEEN 1 AND 200),
    CONSTRAINT dependency_advisory_sources_url_shape CHECK (char_length(url) <= 2048),
    CONSTRAINT dependency_advisory_sources_license_shape CHECK (char_length(license) <= 200),
    CONSTRAINT dependency_advisory_sources_attribution_shape CHECK (char_length(attribution) <= 500),
    CONSTRAINT dependency_advisory_sources_last_sync_status_known CHECK (last_sync_status IN ('never', 'running', 'success', 'failed')),
    CONSTRAINT dependency_advisory_sources_last_sync_error_shape CHECK (char_length(last_sync_error) <= 2000),
    CONSTRAINT dependency_advisory_sources_cursor_value_shape CHECK (char_length(cursor_value) <= 1024),
    CONSTRAINT dependency_advisory_sources_etag_shape CHECK (char_length(etag) <= 512),
    CONSTRAINT dependency_advisory_sources_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_advisory_sources
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE dependency_advisory_aliases (
    id          bigserial   PRIMARY KEY,
    advisory_id bigint      NOT NULL REFERENCES dependency_advisories(id) ON DELETE CASCADE,
    alias_kind  text        NOT NULL,
    alias_value text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_advisory_aliases_alias_kind_shape CHECK (char_length(alias_kind) BETWEEN 1 AND 40),
    CONSTRAINT dependency_advisory_aliases_alias_value_shape CHECK (char_length(alias_value) BETWEEN 1 AND 160)
);

CREATE UNIQUE INDEX dependency_advisory_aliases_unique_idx
    ON dependency_advisory_aliases (advisory_id, alias_kind, lower(alias_value));

CREATE INDEX dependency_advisory_aliases_lookup_idx
    ON dependency_advisory_aliases (alias_kind, lower(alias_value));

CREATE TABLE dependency_advisory_affected_ranges (
    id               bigserial   PRIMARY KEY,
    advisory_id      bigint      NOT NULL REFERENCES dependency_advisories(id) ON DELETE CASCADE,
    ecosystem        text        NOT NULL,
    package_name     text        NOT NULL,
    range_expression text        NOT NULL DEFAULT '',
    introduced       text        NOT NULL DEFAULT '',
    fixed            text        NOT NULL DEFAULT '',
    last_affected    text        NOT NULL DEFAULT '',
    metadata         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_advisory_affected_ranges_ecosystem_shape CHECK (char_length(ecosystem) BETWEEN 1 AND 40),
    CONSTRAINT dependency_advisory_affected_ranges_package_name_shape CHECK (char_length(package_name) BETWEEN 1 AND 512),
    CONSTRAINT dependency_advisory_affected_ranges_range_expression_shape CHECK (char_length(range_expression) <= 500),
    CONSTRAINT dependency_advisory_affected_ranges_introduced_shape CHECK (char_length(introduced) <= 255),
    CONSTRAINT dependency_advisory_affected_ranges_fixed_shape CHECK (char_length(fixed) <= 255),
    CONSTRAINT dependency_advisory_affected_ranges_last_affected_shape CHECK (char_length(last_affected) <= 255),
    CONSTRAINT dependency_advisory_affected_ranges_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE UNIQUE INDEX dependency_advisory_affected_ranges_unique_idx
    ON dependency_advisory_affected_ranges (
        advisory_id, ecosystem, lower(package_name),
        range_expression, introduced, fixed, last_affected
    );

CREATE INDEX dependency_advisory_affected_ranges_package_idx
    ON dependency_advisory_affected_ranges (ecosystem, lower(package_name));

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_advisory_affected_ranges
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE dependency_advisory_sync_runs (
    id              bigserial   PRIMARY KEY,
    source_name     text        NOT NULL,
    status          text        NOT NULL DEFAULT 'running',
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz NULL,
    advisory_count  int         NOT NULL DEFAULT 0,
    upserted_count  int         NOT NULL DEFAULT 0,
    withdrawn_count int         NOT NULL DEFAULT 0,
    error_message   text        NOT NULL DEFAULT '',
    metadata        jsonb       NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT dependency_advisory_sync_runs_source_name_shape CHECK (char_length(source_name) BETWEEN 1 AND 80),
    CONSTRAINT dependency_advisory_sync_runs_status_known CHECK (status IN ('running', 'success', 'failed')),
    CONSTRAINT dependency_advisory_sync_runs_counts_nonnegative CHECK (
        advisory_count >= 0 AND upserted_count >= 0 AND withdrawn_count >= 0
    ),
    CONSTRAINT dependency_advisory_sync_runs_error_message_shape CHECK (char_length(error_message) <= 2000),
    CONSTRAINT dependency_advisory_sync_runs_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX dependency_advisory_sync_runs_source_idx
    ON dependency_advisory_sync_runs (source_name, started_at DESC);

-- +goose Down
DROP TABLE IF EXISTS dependency_advisory_sync_runs;
DROP TABLE IF EXISTS dependency_advisory_affected_ranges;
DROP TABLE IF EXISTS dependency_advisory_aliases;
DROP TABLE IF EXISTS dependency_advisory_sources;

ALTER TABLE dependency_advisories
    DROP CONSTRAINT IF EXISTS dependency_advisories_cwe_ids_array,
    DROP CONSTRAINT IF EXISTS dependency_advisories_cvss_vector_shape,
    DROP CONSTRAINT IF EXISTS dependency_advisories_cvss_score_range,
    DROP CONSTRAINT IF EXISTS dependency_advisories_source_url_shape,
    DROP COLUMN IF EXISTS cwe_ids,
    DROP COLUMN IF EXISTS cvss_vector,
    DROP COLUMN IF EXISTS cvss_score,
    DROP COLUMN IF EXISTS source_url,
    DROP COLUMN IF EXISTS modified_at;
