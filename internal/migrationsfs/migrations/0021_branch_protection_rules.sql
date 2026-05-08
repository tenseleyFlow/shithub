-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Branch protection rules. Each rule is one (repo, glob pattern) tuple
-- with a set of bool/array fields the pre-receive hook checks. Pattern
-- matching is glob-style (filepath.Match: `*`, `?`, `[abc]`) — matches
-- GitHub's UX and is easy to author.
--
-- Some columns are placeholders for later sprints:
--   require_signed_commits   — post-MVP signing surface
--   require_pr_for_push      — post-MVP "no direct push" enforcement
--   status_checks_required   — S24 ships the check engine
--
-- The pre-receive hook ignores those columns until their owning sprint
-- wires real enforcement; the schema is forward-compatible.

-- +goose Up
CREATE TABLE branch_protection_rules (
    id                          bigserial   PRIMARY KEY,
    repo_id                     bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    pattern                     text        NOT NULL,
    prevent_force_push          boolean     NOT NULL DEFAULT true,
    prevent_deletion            boolean     NOT NULL DEFAULT true,
    require_pr_for_push         boolean     NOT NULL DEFAULT false,
    allowed_pusher_user_ids     bigint[]    NOT NULL DEFAULT '{}',
    require_signed_commits      boolean     NOT NULL DEFAULT false,
    status_checks_required      text[]      NOT NULL DEFAULT '{}',
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    created_by_user_id          bigint      REFERENCES users(id) ON DELETE SET NULL,

    CONSTRAINT branch_protection_rules_pattern_length CHECK (char_length(pattern) BETWEEN 1 AND 200)
);

CREATE INDEX branch_protection_rules_repo_idx ON branch_protection_rules (repo_id, pattern);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON branch_protection_rules
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS branch_protection_rules;
