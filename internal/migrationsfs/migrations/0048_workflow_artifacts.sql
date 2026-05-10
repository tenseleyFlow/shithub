-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow artifacts — uploaded blobs from `shithub/upload-artifact`
-- magic step.
--
-- Each row references the run (not the job) so re-runs can produce a
-- fresh artifact set; (run_id, name) is unique per run. object_key is
-- the Spaces key (under runs/<run_id>/artifacts/<name>); the blob
-- itself lives in object storage, not Postgres.
--
-- expires_at defaults to 90 days from creation; the retention sweep
-- (S41g) deletes Spaces blobs past their expiry and prunes rows.
-- Per-run override is supported via the upload-artifact `retention-days`
-- input.
--
-- The runner uses signed S3 PUT URLs (S41c artifact upload endpoint)
-- to upload directly without proxying through shithubd-web. The
-- byte_count is reported back after upload completes (S41d).

-- +goose Up

CREATE TABLE workflow_artifacts (
    id          bigserial    PRIMARY KEY,
    run_id      bigint       NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    name        text         NOT NULL,
    object_key  text         NOT NULL,
    byte_count  bigint       NOT NULL DEFAULT 0,
    expires_at  timestamptz  NOT NULL DEFAULT (now() + interval '90 days'),
    created_at  timestamptz  NOT NULL DEFAULT now(),

    UNIQUE (run_id, name),

    CONSTRAINT workflow_artifacts_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT workflow_artifacts_name_format CHECK (
        name ~ '^[A-Za-z0-9._-]+$' AND name NOT LIKE '..%' AND name NOT LIKE '%/%'
    ),
    CONSTRAINT workflow_artifacts_object_key_length CHECK (char_length(object_key) BETWEEN 1 AND 1024),
    CONSTRAINT workflow_artifacts_byte_count_nonneg CHECK (byte_count >= 0)
);

CREATE INDEX workflow_artifacts_run_idx     ON workflow_artifacts (run_id);
CREATE INDEX workflow_artifacts_expires_idx ON workflow_artifacts (expires_at);


-- +goose Down
DROP TABLE IF EXISTS workflow_artifacts;
