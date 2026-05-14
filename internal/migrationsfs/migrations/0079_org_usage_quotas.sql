-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS SP08 — organization usage accounting and quota overrides.
--
-- This migration adds the durable counters needed before quota gates can
-- safely reject hosted-cost writes. Stripe remains billing's payment source of
-- truth; these tables are shithub's local usage source of truth.

-- +goose Up

CREATE TYPE org_quota_kind AS ENUM (
    'storage_bytes',
    'actions_minutes'
);

CREATE TABLE org_usage_counters (
    org_id                   bigint      PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    repo_storage_bytes       bigint      NOT NULL DEFAULT 0,
    object_storage_bytes     bigint      NOT NULL DEFAULT 0,
    actions_log_bytes        bigint      NOT NULL DEFAULT 0,
    actions_artifact_bytes   bigint      NOT NULL DEFAULT 0,
    actions_minutes_used     bigint      NOT NULL DEFAULT 0,
    actions_period_start     timestamptz NOT NULL DEFAULT date_trunc('month', now()),
    actions_period_end       timestamptz NOT NULL DEFAULT (date_trunc('month', now()) + interval '1 month'),
    calculated_at            timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT org_usage_counters_nonnegative CHECK (
        repo_storage_bytes >= 0
        AND object_storage_bytes >= 0
        AND actions_log_bytes >= 0
        AND actions_artifact_bytes >= 0
        AND actions_minutes_used >= 0
    ),
    CONSTRAINT org_usage_counters_period_order CHECK (actions_period_start < actions_period_end)
);

CREATE INDEX org_usage_counters_storage_idx
    ON org_usage_counters ((repo_storage_bytes + object_storage_bytes) DESC);

CREATE INDEX org_usage_counters_actions_idx
    ON org_usage_counters (actions_period_start, actions_period_end, actions_minutes_used DESC);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON org_usage_counters
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

INSERT INTO org_usage_counters (org_id)
SELECT id
FROM orgs
ON CONFLICT (org_id) DO NOTHING;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_org_usage_counters_seed() RETURNS trigger AS $$
BEGIN
    INSERT INTO org_usage_counters (org_id)
    VALUES (NEW.id)
    ON CONFLICT (org_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER tg_org_usage_counters_seed_ai
    AFTER INSERT ON orgs
    FOR EACH ROW EXECUTE FUNCTION tg_org_usage_counters_seed();

CREATE TABLE org_usage_snapshots (
    id                       bigserial   PRIMARY KEY,
    org_id                   bigint      NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    source                   text        NOT NULL DEFAULT 'local',
    repo_storage_bytes       bigint      NOT NULL,
    object_storage_bytes     bigint      NOT NULL,
    actions_log_bytes        bigint      NOT NULL,
    actions_artifact_bytes   bigint      NOT NULL,
    actions_minutes_used     bigint      NOT NULL,
    actions_period_start     timestamptz NOT NULL,
    actions_period_end       timestamptz NOT NULL,
    captured_at              timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT org_usage_snapshots_source_length CHECK (char_length(source) BETWEEN 1 AND 64),
    CONSTRAINT org_usage_snapshots_nonnegative CHECK (
        repo_storage_bytes >= 0
        AND object_storage_bytes >= 0
        AND actions_log_bytes >= 0
        AND actions_artifact_bytes >= 0
        AND actions_minutes_used >= 0
    ),
    CONSTRAINT org_usage_snapshots_period_order CHECK (actions_period_start < actions_period_end)
);

CREATE INDEX org_usage_snapshots_org_captured_idx
    ON org_usage_snapshots (org_id, captured_at DESC);

CREATE TABLE org_quota_overrides (
    org_id              bigint         NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    kind                org_quota_kind NOT NULL,
    limit_value         bigint,
    unlimited           boolean        NOT NULL DEFAULT false,
    note                text           NOT NULL DEFAULT '',
    created_by_user_id  bigint         REFERENCES users(id) ON DELETE SET NULL,
    created_at          timestamptz    NOT NULL DEFAULT now(),
    updated_at          timestamptz    NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, kind),

    CONSTRAINT org_quota_overrides_value_shape CHECK (
        (unlimited = true AND limit_value IS NULL)
        OR (unlimited = false AND limit_value IS NOT NULL AND limit_value >= 0)
    ),
    CONSTRAINT org_quota_overrides_note_length CHECK (char_length(note) <= 500)
);

CREATE INDEX org_quota_overrides_kind_idx
    ON org_quota_overrides (kind, updated_at DESC);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON org_quota_overrides
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS set_updated_at ON org_quota_overrides;
DROP INDEX IF EXISTS org_quota_overrides_kind_idx;
DROP TABLE IF EXISTS org_quota_overrides;

DROP INDEX IF EXISTS org_usage_snapshots_org_captured_idx;
DROP TABLE IF EXISTS org_usage_snapshots;

DROP TRIGGER IF EXISTS tg_org_usage_counters_seed_ai ON orgs;
DROP FUNCTION IF EXISTS tg_org_usage_counters_seed();
DROP TRIGGER IF EXISTS set_updated_at ON org_usage_counters;
DROP INDEX IF EXISTS org_usage_counters_actions_idx;
DROP INDEX IF EXISTS org_usage_counters_storage_idx;
DROP TABLE IF EXISTS org_usage_counters;

DROP TYPE IF EXISTS org_quota_kind;
