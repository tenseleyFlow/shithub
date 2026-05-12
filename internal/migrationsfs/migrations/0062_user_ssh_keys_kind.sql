-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Add a `kind` discriminator to user_ssh_keys so callers can register
-- a key for git authentication (authorized-keys lookup) or for git
-- commit signing without conflating the two intents.
--
-- Pre-existing rows are git-auth keys (the only kind shithub recognised
-- through S07-S16); we backfill `authentication` and let the check
-- constraint enforce the closed set going forward.
--
-- The /api/v1/user/keys REST surface (S50 §1) returns rows filtered to
-- `authentication` by default to mirror GitHub's /user/keys shape; the
-- signing surface lands behind /user/ssh_signing_keys when a CLI sprint
-- consumes it.

-- +goose Up
ALTER TABLE user_ssh_keys
    ADD COLUMN kind text NOT NULL DEFAULT 'authentication'
    CHECK (kind IN ('authentication', 'signing'));

CREATE INDEX user_ssh_keys_user_id_kind_idx
    ON user_ssh_keys (user_id, kind, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS user_ssh_keys_user_id_kind_idx;
ALTER TABLE user_ssh_keys DROP COLUMN IF EXISTS kind;
