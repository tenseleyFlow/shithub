-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Stars + the cached repos.star_count column + the trigger that
-- maintains it.
--
-- The S11 status block claimed `repos.star_count` was already in
-- place; it was not. We add the column here so this sprint stands
-- alone (noted in the S26 status block).
--
-- Deletion semantics: a star row is the user's choice. When the user
-- is hard-deleted (or the repo is hard-deleted) the row goes with
-- them via ON DELETE CASCADE; the AFTER DELETE trigger keeps
-- star_count consistent.

-- +goose Up
ALTER TABLE repos
    ADD COLUMN star_count    bigint NOT NULL DEFAULT 0,
    ADD COLUMN watcher_count bigint NOT NULL DEFAULT 0;

CREATE TABLE stars (
    user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id    bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    starred_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, repo_id)
);

-- Stars-of-a-user, recency-sorted: drives `/{user}?tab=stars`.
CREATE INDEX stars_user_starred_at_idx
    ON stars (user_id, starred_at DESC);

-- Stargazers-of-a-repo, recency-sorted: drives `/{owner}/{repo}/stargazers`.
CREATE INDEX stars_repo_starred_at_idx
    ON stars (repo_id, starred_at DESC);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_stars_count_inc() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    UPDATE repos SET star_count = star_count + 1 WHERE id = NEW.repo_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_stars_count_dec() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    UPDATE repos SET star_count = GREATEST(star_count - 1, 0)
        WHERE id = OLD.repo_id;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER stars_count_inc AFTER INSERT ON stars
    FOR EACH ROW EXECUTE FUNCTION tg_stars_count_inc();

CREATE TRIGGER stars_count_dec AFTER DELETE ON stars
    FOR EACH ROW EXECUTE FUNCTION tg_stars_count_dec();

-- +goose Down
DROP TRIGGER IF EXISTS stars_count_dec ON stars;
DROP TRIGGER IF EXISTS stars_count_inc ON stars;
DROP FUNCTION IF EXISTS tg_stars_count_dec();
DROP FUNCTION IF EXISTS tg_stars_count_inc();
DROP TABLE IF EXISTS stars;
ALTER TABLE repos
    DROP COLUMN IF EXISTS watcher_count,
    DROP COLUMN IF EXISTS star_count;
