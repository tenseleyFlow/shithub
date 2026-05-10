-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Profile and organization pinned repositories.
--
-- A pin set records that the owner has customized their pinned
-- repositories. The separate row lets us distinguish "never customized"
-- from "customized to zero pins" while keeping the ordered pins table
-- normalized and easy to replace transactionally.

-- +goose Up
CREATE TABLE profile_pin_sets (
    id            bigserial   PRIMARY KEY,
    owner_user_id bigint      REFERENCES users(id) ON DELETE CASCADE,
    owner_org_id  bigint      REFERENCES orgs(id) ON DELETE CASCADE,
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT profile_pin_sets_owner_xor CHECK (
        (owner_user_id IS NOT NULL AND owner_org_id IS NULL)
     OR (owner_user_id IS NULL     AND owner_org_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX profile_pin_sets_owner_user_uq
    ON profile_pin_sets (owner_user_id)
    WHERE owner_user_id IS NOT NULL;

CREATE UNIQUE INDEX profile_pin_sets_owner_org_uq
    ON profile_pin_sets (owner_org_id)
    WHERE owner_org_id IS NOT NULL;

CREATE TABLE profile_pins (
    set_id    bigint      NOT NULL REFERENCES profile_pin_sets(id) ON DELETE CASCADE,
    repo_id   bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    position  integer     NOT NULL,
    pinned_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (set_id, repo_id),
    CONSTRAINT profile_pins_position_range CHECK (position BETWEEN 1 AND 6),
    CONSTRAINT profile_pins_set_position_uq UNIQUE (set_id, position)
);

CREATE INDEX profile_pins_repo_idx ON profile_pins (repo_id);

-- +goose Down
DROP TABLE IF EXISTS profile_pins;
DROP TABLE IF EXISTS profile_pin_sets;
