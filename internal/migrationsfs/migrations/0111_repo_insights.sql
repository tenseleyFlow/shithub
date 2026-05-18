-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
CREATE TABLE repo_insight_snapshots (
    repo_id bigint PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    default_branch text NOT NULL,
    head_sha text NOT NULL DEFAULT '',
    captured_at timestamptz NOT NULL DEFAULT now(),
    commit_count integer NOT NULL DEFAULT 0 CHECK (commit_count >= 0),
    contributor_count integer NOT NULL DEFAULT 0 CHECK (contributor_count >= 0),
    additions bigint NOT NULL DEFAULT 0 CHECK (additions >= 0),
    deletions bigint NOT NULL DEFAULT 0 CHECK (deletions >= 0),
    data jsonb NOT NULL,
    CONSTRAINT repo_insight_snapshots_data_object CHECK (jsonb_typeof(data) = 'object')
);

CREATE INDEX repo_insight_snapshots_captured_idx
    ON repo_insight_snapshots (captured_at DESC);

-- +goose Down
DROP TABLE IF EXISTS repo_insight_snapshots;
