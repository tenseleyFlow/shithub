-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S42 — Social feed and trending.
--
-- Follows are user-initiated social edges. S42's core contract is
-- user->user follows; shithub also supports user->org follows so org
-- activity can participate in the same feed without a later table split.
--
-- Trending snapshots store denormalized rankings produced by the worker.
-- The web UI may fall back to live rankings when no snapshot exists yet.

-- +goose Up

CREATE TABLE follows (
    id               bigserial   PRIMARY KEY,
    follower_user_id bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_user_id bigint      REFERENCES users(id) ON DELETE CASCADE,
    followee_org_id  bigint      REFERENCES orgs(id) ON DELETE CASCADE,
    followed_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT follows_target_xor CHECK (
        (followee_user_id IS NOT NULL AND followee_org_id IS NULL)
     OR (followee_user_id IS NULL     AND followee_org_id IS NOT NULL)
    ),
    CONSTRAINT follows_no_self_user CHECK (
        followee_user_id IS NULL OR followee_user_id <> follower_user_id
    )
);

CREATE UNIQUE INDEX follows_user_target_unique
    ON follows (follower_user_id, followee_user_id)
    WHERE followee_user_id IS NOT NULL;

CREATE UNIQUE INDEX follows_org_target_unique
    ON follows (follower_user_id, followee_org_id)
    WHERE followee_org_id IS NOT NULL;

CREATE INDEX follows_following_idx
    ON follows (follower_user_id, followed_at DESC);

CREATE INDEX follows_user_followers_idx
    ON follows (followee_user_id, followed_at DESC)
    WHERE followee_user_id IS NOT NULL;

CREATE INDEX follows_org_followers_idx
    ON follows (followee_org_id, followed_at DESC)
    WHERE followee_org_id IS NOT NULL;

CREATE TYPE trending_scope AS ENUM ('day', 'week', 'month');
CREATE TYPE trending_kind AS ENUM ('repos', 'users');

CREATE TABLE trending_snapshots (
    id          bigserial       PRIMARY KEY,
    scope       trending_scope  NOT NULL,
    kind        trending_kind   NOT NULL,
    captured_at timestamptz     NOT NULL DEFAULT now(),
    payload     jsonb           NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT trending_snapshots_payload_object_or_array CHECK (
        jsonb_typeof(payload) IN ('object', 'array')
    )
);

CREATE INDEX trending_snapshots_latest_idx
    ON trending_snapshots (scope, kind, captured_at DESC);

-- +goose Down
DROP INDEX IF EXISTS trending_snapshots_latest_idx;
DROP TABLE IF EXISTS trending_snapshots;
DROP TYPE IF EXISTS trending_kind;
DROP TYPE IF EXISTS trending_scope;

DROP INDEX IF EXISTS follows_org_followers_idx;
DROP INDEX IF EXISTS follows_user_followers_idx;
DROP INDEX IF EXISTS follows_following_idx;
DROP INDEX IF EXISTS follows_org_target_unique;
DROP INDEX IF EXISTS follows_user_target_unique;
DROP TABLE IF EXISTS follows;
