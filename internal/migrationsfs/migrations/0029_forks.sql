-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S27 fork-support columns + the maintenance trigger.
--
-- The S11 / S27 specs assumed `is_fork` and `fork_count` were already
-- in `repos`; in fact only `fork_of_repo_id` shipped. We add
-- `fork_count` here. `is_fork` is intentionally skipped — it would
-- duplicate the truth of `fork_of_repo_id IS NOT NULL`. Same gap
-- pattern as S26 caught for `star_count`/`watcher_count`; noted in
-- the S27 status block.
--
-- `init_status` tracks the async clone job's progression. Synchronous
-- repo creates (the S11 path) write 'initialized' directly. Forks
-- start at 'init_pending' and the worker flips to 'initialized' on
-- success or 'init_failed' on permanent failure (poison error). The
-- repo home view reads this column to decide between "your fork is
-- being prepared" placeholder and the real tree view.

-- +goose Up
ALTER TABLE repos
    ADD COLUMN fork_count bigint NOT NULL DEFAULT 0;

CREATE TYPE repo_init_status AS ENUM ('initialized', 'init_pending', 'init_failed');

-- Default 'initialized' so the back-fill on existing rows is correct
-- (every pre-S27 repo was created synchronously).
ALTER TABLE repos
    ADD COLUMN init_status repo_init_status NOT NULL DEFAULT 'initialized';

-- Maintenance trigger: when a repos row with fork_of_repo_id IS NOT NULL
-- is inserted (or hard-deleted), bump the source repo's fork_count.
-- Soft delete (deleted_at IS NOT NULL) does NOT decrement — the row
-- is still present, hard-delete is what cascades. ON DELETE SET NULL
-- on `fork_of_repo_id` would NULL the column on source delete; the
-- delete trigger fires before that on the *fork* row.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_forks_count_inc() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.fork_of_repo_id IS NOT NULL THEN
        UPDATE repos SET fork_count = fork_count + 1
            WHERE id = NEW.fork_of_repo_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_forks_count_dec() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.fork_of_repo_id IS NOT NULL THEN
        UPDATE repos SET fork_count = GREATEST(fork_count - 1, 0)
            WHERE id = OLD.fork_of_repo_id;
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER forks_count_inc AFTER INSERT ON repos
    FOR EACH ROW EXECUTE FUNCTION tg_forks_count_inc();

CREATE TRIGGER forks_count_dec AFTER DELETE ON repos
    FOR EACH ROW EXECUTE FUNCTION tg_forks_count_dec();

-- Listing forks of a repo: index on the FK + recency.
CREATE INDEX repos_fork_of_repo_id_idx
    ON repos (fork_of_repo_id, created_at DESC)
    WHERE fork_of_repo_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS repos_fork_of_repo_id_idx;
DROP TRIGGER IF EXISTS forks_count_dec ON repos;
DROP TRIGGER IF EXISTS forks_count_inc ON repos;
DROP FUNCTION IF EXISTS tg_forks_count_dec();
DROP FUNCTION IF EXISTS tg_forks_count_inc();
ALTER TABLE repos
    DROP COLUMN IF EXISTS init_status,
    DROP COLUMN IF EXISTS fork_count;
DROP TYPE IF EXISTS repo_init_status;
