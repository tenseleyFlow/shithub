-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Retention indexes for the repo traffic tables (availability campaign,
-- 2026-09-02 sitrep root cause #7).
--
-- 0112 shipped these tables with no purge and no index that a purge can
-- use. In production they had grown to 881 MB of a 988 MB database:
-- 1.26 M rows in repo_traffic_paths and 1.34 M in repo_traffic_uniques,
-- ~30 k/day/table, most of it crawler paths. The `traffic:purge` worker
-- job now trims them to a 30-day window; without a day index every
-- batch of that purge would be a sequential scan of the whole table.
--
-- repo_traffic_uniques is deliberately absent: it already carries
-- repo_traffic_uniques_created_idx (created_at), and created_at is set
-- on the same request that derives `day`, so the purge filters that
-- table on created_at and reuses the existing index rather than paying
-- for a second one on a table that takes an insert per pageview.
--
-- repo_traffic_daily already has repo_traffic_daily_day_idx (day DESC),
-- which serves its (much longer) retention window.
--
-- CONCURRENTLY, because repo_traffic_paths and repo_traffic_referrers
-- are written synchronously in the request path — a plain CREATE INDEX
-- would block pageviews for the length of the build. That forces
-- NO TRANSACTION. If a build is interrupted Postgres leaves an INVALID
-- index behind; drop it by name and re-run the migration.
--
-- No bulk DELETE here on purpose. Pruning 2.5 M rows inside a migration
-- would hold one long transaction during a deploy, which is the failure
-- mode this campaign is trying to remove. The first run of
-- `traffic:purge` does the backfill in bounded batches instead.

-- +goose NO TRANSACTION

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS repo_traffic_paths_day_idx
    ON repo_traffic_paths (day);

CREATE INDEX CONCURRENTLY IF NOT EXISTS repo_traffic_referrers_day_idx
    ON repo_traffic_referrers (day);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS repo_traffic_referrers_day_idx;
DROP INDEX CONCURRENTLY IF EXISTS repo_traffic_paths_day_idx;
