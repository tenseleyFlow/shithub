-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-11a: PAT IP allowlist.
--
-- Pro users can attach an array of CIDR ranges to a PAT; the auth
-- middleware rejects requests whose trusted client IP falls outside
-- the allowlist. Empty array means "no IP restriction" (every Free
-- token, plus Pro tokens the user didn't restrict).
--
-- SECURITY CONTRACT:
-- 1. The allowlist is compared against the *trusted* client IP, not
--    the raw X-Forwarded-For. The middleware sources the IP from
--    chi/middleware/RealIP (which honors only the operator-configured
--    trusted-proxy set).
-- 2. Adding to the allowlist is permission-checked: the form path
--    consults FeatureFineGrainedPATs before persisting.
-- 3. Empty array = no restriction. We do NOT allow null because then
--    "null != empty array" becomes a load-bearing distinction we'd
--    have to defend everywhere.

-- +goose Up
ALTER TABLE user_tokens
    ADD COLUMN ip_allowlist text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE user_tokens
    DROP COLUMN ip_allowlist;
