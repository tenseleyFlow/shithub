-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-11b: PAT single-repo binding.
--
-- A bound token may only authenticate requests that target the specific
-- repo it's tied to. Unbound tokens (the default + the entirety of the
-- Free tier) behave exactly as before — the binding is opt-in.
--
-- Security contract (read this if you're touching the column):
--
--   1. NULL repo_id = no binding = no restriction. The middleware
--      never short-circuits on a NULL repo_id. Free users (and any
--      Pro user who didn't attach a binding) see no behavior change.
--
--   2. Non-NULL repo_id = the token is bound to that single repo.
--      Downstream callers that resolve a repo for the request MUST
--      verify the binding matches before serving repo-scoped data.
--      The pat.RepoBindingAllows helper does this comparison.
--
--   3. ON DELETE CASCADE: deleting a repo also deletes any tokens
--      bound to it. The alternative (set NULL) would silently widen
--      access — the user expressed intent to scope to that repo, and
--      if the repo is gone the bound tokens should be gone too.
--
-- +goose Up
ALTER TABLE user_tokens
    ADD COLUMN repo_id bigint REFERENCES repos(id) ON DELETE CASCADE;

-- Partial index — only tokens with a binding need to be lookup-able
-- by repo (e.g. for an admin "tokens bound to repo X" report). NULL
-- repo_id is the common case and we don't want to bloat the index.
CREATE INDEX user_tokens_repo_id_idx
    ON user_tokens (repo_id)
    WHERE repo_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS user_tokens_repo_id_idx;
ALTER TABLE user_tokens
    DROP COLUMN repo_id;
