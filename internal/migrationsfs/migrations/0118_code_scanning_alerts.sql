-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP27: non-AI code security. SARIF uploads are normalized into
-- deduplicated repository alerts so repo/org security pages can render
-- findings without exposing raw analysis artifacts.

-- +goose Up
CREATE TABLE code_scanning_uploads (
    id               bigserial   PRIMARY KEY,
    repo_id          bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    tool_name        text        NOT NULL,
    tool_guid        text        NOT NULL DEFAULT '',
    category         text        NOT NULL DEFAULT '',
    commit_sha       text        NOT NULL,
    ref_name         text        NOT NULL DEFAULT '',
    alert_count      int         NOT NULL DEFAULT 0,
    raw_sarif_sha256 text        NOT NULL DEFAULT '',
    uploaded_by      bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    uploaded_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT code_scanning_uploads_tool_name_shape CHECK (char_length(tool_name) BETWEEN 1 AND 160),
    CONSTRAINT code_scanning_uploads_tool_guid_shape CHECK (char_length(tool_guid) <= 160),
    CONSTRAINT code_scanning_uploads_category_shape CHECK (char_length(category) <= 160),
    CONSTRAINT code_scanning_uploads_commit_sha_shape CHECK (char_length(commit_sha) BETWEEN 1 AND 128),
    CONSTRAINT code_scanning_uploads_ref_name_shape CHECK (char_length(ref_name) <= 255),
    CONSTRAINT code_scanning_uploads_alert_count_nonnegative CHECK (alert_count >= 0),
    CONSTRAINT code_scanning_uploads_raw_sarif_sha256_shape CHECK (raw_sarif_sha256 = '' OR raw_sarif_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX code_scanning_uploads_repo_uploaded_idx
    ON code_scanning_uploads (repo_id, uploaded_at DESC);

CREATE TABLE code_scanning_alerts (
    id             bigserial   PRIMARY KEY,
    repo_id        bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    tool_name      text        NOT NULL,
    tool_guid      text        NOT NULL DEFAULT '',
    rule_id        text        NOT NULL,
    rule_name      text        NOT NULL DEFAULT '',
    severity       text        NOT NULL,
    message        text        NOT NULL,
    path           text        NOT NULL,
    start_line     int         NOT NULL DEFAULT 1,
    end_line       int         NOT NULL DEFAULT 0,
    start_column   int         NOT NULL DEFAULT 0,
    end_column     int         NOT NULL DEFAULT 0,
    fingerprint    text        NOT NULL,
    commit_sha     text        NOT NULL DEFAULT '',
    ref_name       text        NOT NULL DEFAULT '',
    status         text        NOT NULL DEFAULT 'open',
    dismissal_note text        NOT NULL DEFAULT '',
    dismissed_by   bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    dismissed_at   timestamptz NULL,
    fixed_at       timestamptz NULL,
    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT code_scanning_alerts_tool_name_shape CHECK (char_length(tool_name) BETWEEN 1 AND 160),
    CONSTRAINT code_scanning_alerts_tool_guid_shape CHECK (char_length(tool_guid) <= 160),
    CONSTRAINT code_scanning_alerts_rule_id_shape CHECK (char_length(rule_id) BETWEEN 1 AND 512),
    CONSTRAINT code_scanning_alerts_rule_name_shape CHECK (char_length(rule_name) <= 512),
    CONSTRAINT code_scanning_alerts_severity_known CHECK (severity IN ('low', 'moderate', 'high', 'critical')),
    CONSTRAINT code_scanning_alerts_message_shape CHECK (char_length(message) BETWEEN 1 AND 2000),
    CONSTRAINT code_scanning_alerts_path_shape CHECK (char_length(path) BETWEEN 1 AND 1024),
    CONSTRAINT code_scanning_alerts_line_bounds CHECK (start_line >= 1 AND end_line >= 0),
    CONSTRAINT code_scanning_alerts_column_bounds CHECK (start_column >= 0 AND end_column >= 0),
    CONSTRAINT code_scanning_alerts_fingerprint_shape CHECK (char_length(fingerprint) BETWEEN 1 AND 128),
    CONSTRAINT code_scanning_alerts_commit_sha_shape CHECK (char_length(commit_sha) <= 128),
    CONSTRAINT code_scanning_alerts_ref_name_shape CHECK (char_length(ref_name) <= 255),
    CONSTRAINT code_scanning_alerts_status_known CHECK (status IN ('open', 'dismissed', 'fixed')),
    CONSTRAINT code_scanning_alerts_dismissal_note_shape CHECK (char_length(dismissal_note) <= 500),
    CONSTRAINT code_scanning_alerts_unique UNIQUE (repo_id, tool_name, rule_id, path, start_line, fingerprint)
);

CREATE INDEX code_scanning_alerts_repo_status_idx
    ON code_scanning_alerts (repo_id, status, last_seen_at DESC);

CREATE INDEX code_scanning_alerts_repo_severity_idx
    ON code_scanning_alerts (repo_id, severity, status)
    WHERE status = 'open';

CREATE TRIGGER set_updated_at BEFORE UPDATE ON code_scanning_alerts
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE code_security_campaigns (
    id          bigserial   PRIMARY KEY,
    repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    state       text        NOT NULL DEFAULT 'open',
    created_by  bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    closed_at   timestamptz NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT code_security_campaigns_title_shape CHECK (char_length(title) BETWEEN 1 AND 200),
    CONSTRAINT code_security_campaigns_description_shape CHECK (char_length(description) <= 2000),
    CONSTRAINT code_security_campaigns_state_known CHECK (state IN ('open', 'closed'))
);

CREATE INDEX code_security_campaigns_repo_state_idx
    ON code_security_campaigns (repo_id, state, updated_at DESC);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON code_security_campaigns
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE code_security_campaign_alerts (
    campaign_id bigint      NOT NULL REFERENCES code_security_campaigns(id) ON DELETE CASCADE,
    alert_id    bigint      NOT NULL REFERENCES code_scanning_alerts(id) ON DELETE CASCADE,
    added_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (campaign_id, alert_id)
);

CREATE INDEX code_security_campaign_alerts_alert_idx
    ON code_security_campaign_alerts (alert_id);

-- +goose Down
DROP TABLE IF EXISTS code_security_campaign_alerts;
DROP TABLE IF EXISTS code_security_campaigns;
DROP TABLE IF EXISTS code_scanning_alerts;
DROP TABLE IF EXISTS code_scanning_uploads;
