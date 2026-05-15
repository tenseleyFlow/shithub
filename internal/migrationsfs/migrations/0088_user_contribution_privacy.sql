-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-09: contribution-graph privacy controls. The existing
-- users.include_private_contributions column stays as the gh-parity
-- master toggle (Free + Pro both use it). This sprint adds Pro-only
-- per-repo opt-outs: a user can choose to exclude specific repos
-- from their contribution-graph aggregation entirely (i.e. commits
-- to those repos don't count toward the heatmap or activity feed).
--
-- The optout table is many-to-many. Per-row PK on (user_id, repo_id)
-- so reads + inserts are idempotent. ON DELETE CASCADE so a deleted
-- repo or user takes its opt-outs with it.

-- +goose Up
CREATE TABLE user_contribution_repo_optouts (
    user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id    bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, repo_id)
);

CREATE INDEX user_contribution_repo_optouts_user_id_idx
    ON user_contribution_repo_optouts (user_id);

-- +goose Down
DROP TABLE IF EXISTS user_contribution_repo_optouts;
