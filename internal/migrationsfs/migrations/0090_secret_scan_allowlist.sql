-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-10c: secret-scan allowlist. When a user knows a finding
-- is a false-positive — e.g. a test fixture in the secretscan package
-- itself — they allowlist the (repo, pattern, path) tuple so future
-- scans skip it.
--
-- Granularity: per (repo, pattern, path). Line is intentionally NOT
-- in the key — a moved/edited file with the same FP shape should
-- still be skipped. Operators can revoke an allowlist row to bring
-- the finding back.

-- +goose Up
CREATE TABLE secret_scan_allowlist (
    id          bigserial   PRIMARY KEY,
    repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    pattern     text        NOT NULL,
    path        text        NOT NULL,
    reason      text        NOT NULL DEFAULT '',
    created_by  bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT secret_scan_allowlist_pattern_shape CHECK (
        char_length(pattern) BETWEEN 1 AND 80
    ),
    CONSTRAINT secret_scan_allowlist_path_shape CHECK (
        char_length(path) BETWEEN 1 AND 1024
    ),
    CONSTRAINT secret_scan_allowlist_reason_shape CHECK (
        char_length(reason) BETWEEN 0 AND 500
    ),
    CONSTRAINT secret_scan_allowlist_unique UNIQUE (repo_id, pattern, path)
);

CREATE INDEX secret_scan_allowlist_repo_idx
    ON secret_scan_allowlist (repo_id);

-- +goose Down
DROP TABLE IF EXISTS secret_scan_allowlist;
