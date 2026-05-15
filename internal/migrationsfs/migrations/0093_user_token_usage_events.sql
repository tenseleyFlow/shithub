-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-11c: per-token usage analytics.
--
-- A row per PAT-authenticated request. The middleware fires a
-- fire-and-forget insert on the same goroutine that already touches
-- last_used, so the hot path stays unblocked.
--
-- Contract:
--   * token_id FK CASCADE → revoking/deleting a token drops its
--     history. Analytics are presentational; the user can't
--     reconstruct usage from anywhere else.
--   * occurred_at + token_id index covers the aggregation queries
--     (count-by-day, top-routes-for-window).
--   * route_prefix is the FIRST THREE path segments at most ("/" +
--     2 segments) — bounded for cardinality and to keep individual
--     resource IDs out of the analytics view.
--   * No request body, no auth header, no client IP — keeps GDPR
--     surface minimal.
--   * Retention is a follow-up. v1 ships without a TTL; the
--     PRO-EXT01-11d sprint slot will add a worker-cleanup job.
--
-- +goose Up
CREATE TABLE user_token_usage_events (
    id bigserial PRIMARY KEY,
    token_id bigint NOT NULL REFERENCES user_tokens(id) ON DELETE CASCADE,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    method text NOT NULL,
    route_prefix text NOT NULL,
    status_code smallint NOT NULL,

    CONSTRAINT user_token_usage_events_method_chk
        CHECK (char_length(method) BETWEEN 1 AND 10),
    CONSTRAINT user_token_usage_events_route_chk
        CHECK (char_length(route_prefix) BETWEEN 1 AND 64),
    CONSTRAINT user_token_usage_events_status_chk
        CHECK (status_code BETWEEN 100 AND 599)
);

CREATE INDEX user_token_usage_events_token_id_occurred_at_idx
    ON user_token_usage_events (token_id, occurred_at DESC);

-- +goose Down
DROP INDEX IF EXISTS user_token_usage_events_token_id_occurred_at_idx;
DROP TABLE IF EXISTS user_token_usage_events;
