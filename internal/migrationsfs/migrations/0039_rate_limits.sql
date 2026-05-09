-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S35 — Generalized rate-limiting + per-/24 signup throttle.
--
--   * rate_limits          — counter table for any (scope, key) pair,
--                            generalizing S05's auth_throttle. The
--                            ratelimit package is the single writer;
--                            auth_throttle stays in place for the
--                            existing auth surface (kept for back-
--                            compat — generalising S05 callers can
--                            land in a follow-up if profiling shows
--                            the dual table to be wasteful).
--
--   * signup_ip_throttle   — per-/24 signup counter. Distinct from
--                            rate_limits because the key is a CIDR
--                            block (not a string). Used to throw a
--                            soft-block at 5 signups/hour and a hard
--                            block at 20/24h, matching the spec's
--                            anti-abuse heuristics. (Captcha gating
--                            is the natural next step for the soft
--                            block; vendor decision is deferred —
--                            the gate stays here as a 429 today.)
--
-- Pruning: a periodic worker (sweep job, S34's worker pool) deletes
-- rows whose window started more than 24h ago. The covering index
-- on window_started_at keeps the prune cheap.

-- +goose Up
CREATE TABLE rate_limits (
    scope             text        NOT NULL,
    key               text        NOT NULL,
    hits              integer     NOT NULL DEFAULT 0,
    window_started_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (scope, key),
    CONSTRAINT rate_limits_scope_length CHECK (char_length(scope) BETWEEN 1 AND 64),
    CONSTRAINT rate_limits_key_length   CHECK (char_length(key)   BETWEEN 1 AND 256)
);

-- Periodic prune scans by window_started_at; partial index on the
-- "old enough to delete" predicate isn't worth it because the cutoff
-- moves continuously.
CREATE INDEX rate_limits_window_started_idx ON rate_limits (window_started_at);

CREATE TABLE signup_ip_throttle (
    -- inet column accepts the CIDR (/24 for v4, /48 for v6) as a
    -- subtype. Storing the network and the rolling counter together.
    cidr              inet        NOT NULL,
    hits              integer     NOT NULL DEFAULT 0,
    window_started_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (cidr)
);

CREATE INDEX signup_ip_throttle_window_started_idx
    ON signup_ip_throttle (window_started_at);

-- +goose Down
DROP TABLE IF EXISTS signup_ip_throttle;
DROP TABLE IF EXISTS rate_limits;
