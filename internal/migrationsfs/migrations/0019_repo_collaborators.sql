-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- repo_collaborators stores per-(repo, user) role grants beyond the
-- owner. The five roles mirror GitHub's:
--
--   read     — clone/fetch a private repo, view issues/pulls
--   triage   — read + manage issue state (close, label, assign)
--   write    — triage + push, branch create, PR create
--   maintain — write + most settings except dangerous ones
--   admin    — maintain + delete/transfer/visibility
--
-- The owner is implicit (effectively `admin` for the purposes of policy)
-- and is not stored here; the owner column on `repos` is the source of
-- truth. A row in this table for the owner would be redundant.
--
-- Org-team grants live in a separate table (S31). The policy package
-- merges both sources when evaluating an action.

-- +goose Up
CREATE TYPE collab_role AS ENUM ('read', 'triage', 'write', 'maintain', 'admin');

CREATE TABLE repo_collaborators (
    repo_id           bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role              collab_role NOT NULL,
    added_at          timestamptz NOT NULL DEFAULT now(),
    added_by_user_id  bigint      REFERENCES users(id) ON DELETE SET NULL,

    PRIMARY KEY (repo_id, user_id)
);

CREATE INDEX repo_collaborators_user_id_idx ON repo_collaborators (user_id);
CREATE INDEX repo_collaborators_repo_id_idx ON repo_collaborators (repo_id);

-- +goose Down
DROP TABLE IF EXISTS repo_collaborators;
DROP TYPE IF EXISTS collab_role;
