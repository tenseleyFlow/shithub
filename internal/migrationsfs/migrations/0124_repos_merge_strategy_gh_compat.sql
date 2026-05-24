-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- I7a (audit-I11) — repos.delete_branch_on_merge.
--
-- gh-compat repo response surfaces a small constellation of merge-
-- strategy toggles. Most (allow_auto_merge, allow_update_branch,
-- use_squash_pr_title_as_default, web_commit_signoff_required, the four
-- *_commit_title/_message format selectors) have no shithub behavior
-- to gate against yet, so the response builder emits gh-compat defaults
-- as constants. delete_branch_on_merge is different — it's a behavior
-- a user toggles on at repo setup and shithub honors at merge time —
-- so the toggle persists as a column. Wiring of the post-merge branch
-- cleanup itself is sprint-scoped separately; this migration ships the
-- storage and zero default so the column is there when the wiring
-- lands.

-- +goose Up

ALTER TABLE repos
    ADD COLUMN delete_branch_on_merge boolean NOT NULL DEFAULT false;

-- +goose Down

ALTER TABLE repos
    DROP COLUMN IF EXISTS delete_branch_on_merge;
