-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25c: repository security advisory workflows. SP25 created a shallow
-- advisory table for org overview counts; this migration turns it into a
-- workflow table with GitHub-like states, external identifiers, references,
-- collaborator records, and event history.

-- +goose Up
ALTER TABLE repo_security_advisories
    DROP CONSTRAINT repo_security_advisories_state_known;

UPDATE repo_security_advisories
SET state = 'withdrawn',
    closed_at = COALESCE(closed_at, now())
WHERE state = 'closed';

ALTER TABLE repo_security_advisories
    ADD COLUMN ghsa_id text NOT NULL DEFAULT '',
    ADD COLUMN cve_id text NOT NULL DEFAULT '',
    ADD COLUMN reference_urls jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN withdrawn_at timestamptz NULL,
    ADD COLUMN archived_at timestamptz NULL,
    ADD CONSTRAINT repo_security_advisories_state_known CHECK (state IN ('draft', 'published', 'withdrawn', 'archived')),
    ADD CONSTRAINT repo_security_advisories_ghsa_id_shape CHECK (char_length(ghsa_id) <= 120),
    ADD CONSTRAINT repo_security_advisories_cve_id_shape CHECK (char_length(cve_id) <= 80),
    ADD CONSTRAINT repo_security_advisories_reference_urls_array CHECK (jsonb_typeof(reference_urls) = 'array');

UPDATE repo_security_advisories
SET withdrawn_at = closed_at
WHERE state = 'withdrawn'
  AND withdrawn_at IS NULL;

CREATE TABLE repo_security_advisory_collaborators (
    advisory_id bigint      NOT NULL REFERENCES repo_security_advisories(id) ON DELETE CASCADE,
    user_id     bigint      NULL REFERENCES users(id) ON DELETE CASCADE,
    team_id     bigint      NULL REFERENCES teams(id) ON DELETE CASCADE,
    role        text        NOT NULL DEFAULT 'read',
    added_by    bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    added_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_security_advisory_collaborators_one_subject CHECK ((user_id IS NULL) <> (team_id IS NULL)),
    CONSTRAINT repo_security_advisory_collaborators_role_known CHECK (role IN ('read', 'write', 'admin')),
    CONSTRAINT repo_security_advisory_collaborators_unique_user UNIQUE (advisory_id, user_id),
    CONSTRAINT repo_security_advisory_collaborators_unique_team UNIQUE (advisory_id, team_id)
);

CREATE INDEX repo_security_advisory_collaborators_user_idx
    ON repo_security_advisory_collaborators (user_id, advisory_id)
    WHERE user_id IS NOT NULL;

CREATE INDEX repo_security_advisory_collaborators_team_idx
    ON repo_security_advisory_collaborators (team_id, advisory_id)
    WHERE team_id IS NOT NULL;

CREATE TABLE repo_security_advisory_events (
    id          bigserial   PRIMARY KEY,
    advisory_id bigint     NOT NULL REFERENCES repo_security_advisories(id) ON DELETE CASCADE,
    repo_id     bigint     NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    actor_id    bigint     NULL REFERENCES users(id) ON DELETE SET NULL,
    event_type  text       NOT NULL,
    old_state   text       NOT NULL DEFAULT '',
    new_state   text       NOT NULL DEFAULT '',
    message     text       NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_security_advisory_events_event_type_shape CHECK (char_length(event_type) BETWEEN 1 AND 80),
    CONSTRAINT repo_security_advisory_events_state_shape CHECK (char_length(old_state) <= 40 AND char_length(new_state) <= 40),
    CONSTRAINT repo_security_advisory_events_message_shape CHECK (char_length(message) <= 1000)
);

CREATE INDEX repo_security_advisory_events_advisory_idx
    ON repo_security_advisory_events (advisory_id, created_at DESC, id DESC);

CREATE INDEX repo_security_advisory_events_repo_idx
    ON repo_security_advisory_events (repo_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS repo_security_advisory_events;
DROP TABLE IF EXISTS repo_security_advisory_collaborators;

ALTER TABLE repo_security_advisories
    DROP CONSTRAINT repo_security_advisories_reference_urls_array,
    DROP CONSTRAINT repo_security_advisories_cve_id_shape,
    DROP CONSTRAINT repo_security_advisories_ghsa_id_shape,
    DROP CONSTRAINT repo_security_advisories_state_known;

UPDATE repo_security_advisories
SET state = 'closed',
    closed_at = COALESCE(closed_at, withdrawn_at, archived_at, now())
WHERE state IN ('withdrawn', 'archived');

ALTER TABLE repo_security_advisories
    DROP COLUMN archived_at,
    DROP COLUMN withdrawn_at,
    DROP COLUMN reference_urls,
    DROP COLUMN cve_id,
    DROP COLUMN ghsa_id,
    ADD CONSTRAINT repo_security_advisories_state_known CHECK (state IN ('draft', 'published', 'closed'));
