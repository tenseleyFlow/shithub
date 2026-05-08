-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Canonical event log. The S26 spec called for a generic `events`
-- table; the S00-S25 audit's forward-plan finding flagged that S29
-- (notifications) and S33 (webhooks) would also want their own log
-- table. Per the audit's recommendation we land the unified shape
-- here from day 1 — `domain_events` — so S29 and S33 don't have to
-- migrate the schema later.
--
-- Schema columns:
--   actor_user_id — who did it (NULL for system events)
--   kind          — short string identifying the event type
--                   ("star", "unstar", "issue_comment_created", …)
--   repo_id       — the repo the event is scoped to (NULL for
--                   user-scoped events; reserved for org-scoped
--                   events when S30 lands)
--   source_kind   — what kind of object is the source/target
--                   ("repo", "issue", "pull", "user", …)
--   source_id     — the source object's id
--   public        — whether this event is visible in public feeds.
--                   Public-repo events default true; private-repo
--                   events default false; user-scoped events follow
--                   the user's profile-visibility setting.
--   payload       — event-specific JSON. Keep small (<4 KiB);
--                   bigger payloads belong in the source object
--                   (referenced via source_kind/id).
--
-- Read patterns:
--   * notifications fan-out (S29) consumes by polling on
--     created_at >= last_processed.
--   * webhooks (S33) the same.
--   * activity feeds (post-MVP) read public events sliced by repo
--     or by actor.

-- +goose Up
CREATE TABLE domain_events (
    id              bigserial   PRIMARY KEY,
    actor_user_id   bigint      REFERENCES users(id) ON DELETE SET NULL,
    kind            text        NOT NULL,
    repo_id         bigint      REFERENCES repos(id) ON DELETE CASCADE,
    source_kind     text        NOT NULL,
    source_id       bigint      NOT NULL,
    public          boolean     NOT NULL DEFAULT false,
    payload         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT domain_events_kind_length CHECK (char_length(kind) BETWEEN 1 AND 64),
    CONSTRAINT domain_events_source_kind_length CHECK (char_length(source_kind) BETWEEN 1 AND 32)
);

-- Notifications/webhooks consumers poll by created_at; partial-on
-- created_at would gain little since both consumers process every row.
CREATE INDEX domain_events_created_at_idx ON domain_events (created_at);

-- Activity feed: events for a repo, recency-sorted.
CREATE INDEX domain_events_repo_created_idx
    ON domain_events (repo_id, created_at DESC)
    WHERE repo_id IS NOT NULL;

-- Activity feed: events by an actor, recency-sorted.
CREATE INDEX domain_events_actor_created_idx
    ON domain_events (actor_user_id, created_at DESC)
    WHERE actor_user_id IS NOT NULL;

-- Public-feed slice: only public rows, recency-sorted.
CREATE INDEX domain_events_public_created_idx
    ON domain_events (created_at DESC)
    WHERE public = true;

-- +goose Down
DROP TABLE IF EXISTS domain_events;
