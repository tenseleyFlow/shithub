-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Add a `source` discriminator to user_tokens. Tokens minted via the
-- /settings/tokens HTML flow carry 'user_created'; tokens minted via
-- the RFC 8628 device-code Exchange path (internal/auth/devicecode)
-- carry 'oauth_device'. Future sources (oauth_app, service_account)
-- can be added without a migration — the enum lives Go-side in
-- internal/auth/pat so we avoid migration churn on each addition.
--
-- All existing rows backfill to 'user_created' via the column DEFAULT.
-- The /settings/tokens listing and `shithub auth status` surface the
-- value so users can distinguish device-flow grants from manual PATs
-- (a non-recent device-flow user seeing an oauth_device token is a
-- red flag worth surfacing).

-- +goose Up
ALTER TABLE user_tokens
    ADD COLUMN source text NOT NULL DEFAULT 'user_created';

-- +goose Down
ALTER TABLE user_tokens DROP COLUMN IF EXISTS source;
