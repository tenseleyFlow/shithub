-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Per-user, per-repo watch level. Absence of a row is the implicit
-- "participating" default — matches GitHub's semantics. We only
-- materialize a row when the user explicitly sets a level OR when an
-- auto-watch trigger fires (collaborator add → 'all'; first comment /
-- mention / assignment → 'participating').
--
-- watcher_count is the count of rows where level <> 'ignore'. The
-- spec's day-1 lean ("all non-ignore") matches GitHub. We do NOT add
-- collaborators with no row; the auto-watch path inserts a row for
-- them on collab add, so once that fires every collab is in the count.

-- +goose Up
CREATE TYPE watch_level AS ENUM ('all', 'participating', 'ignore');

CREATE TABLE watches (
    user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id    bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    level      watch_level NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, repo_id)
);

-- Watchers-of-a-repo, drives `/{owner}/{repo}/watchers`.
CREATE INDEX watches_repo_idx
    ON watches (repo_id, updated_at DESC)
    WHERE level <> 'ignore';

-- Watches-by-user (e.g. notification fan-out picks recipients).
CREATE INDEX watches_user_idx ON watches (user_id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_watches_count() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    delta int := 0;
    target_repo_id bigint;
BEGIN
    IF TG_OP = 'INSERT' THEN
        target_repo_id := NEW.repo_id;
        IF NEW.level <> 'ignore' THEN
            delta := 1;
        END IF;
    ELSIF TG_OP = 'UPDATE' THEN
        target_repo_id := NEW.repo_id;
        IF OLD.level = 'ignore' AND NEW.level <> 'ignore' THEN
            delta := 1;
        ELSIF OLD.level <> 'ignore' AND NEW.level = 'ignore' THEN
            delta := -1;
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        target_repo_id := OLD.repo_id;
        IF OLD.level <> 'ignore' THEN
            delta := -1;
        END IF;
    END IF;
    IF delta <> 0 THEN
        UPDATE repos
            SET watcher_count = GREATEST(watcher_count + delta, 0)
            WHERE id = target_repo_id;
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER watches_count AFTER INSERT OR UPDATE OR DELETE ON watches
    FOR EACH ROW EXECUTE FUNCTION tg_watches_count();

CREATE TRIGGER set_updated_at BEFORE UPDATE ON watches
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS set_updated_at ON watches;
DROP TRIGGER IF EXISTS watches_count ON watches;
DROP FUNCTION IF EXISTS tg_watches_count();
DROP TABLE IF EXISTS watches;
DROP TYPE IF EXISTS watch_level;
