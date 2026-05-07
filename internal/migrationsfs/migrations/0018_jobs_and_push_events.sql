-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S14 push processing pipeline.
--
-- Three tables + one column:
--   * jobs                     — Postgres-backed work queue with
--                                FOR UPDATE SKIP LOCKED dispatch.
--   * push_events              — one row per ref pushed, written by the
--                                post-receive hook; consumed by the
--                                push:process job.
--   * webhook_events_pending   — accumulator drained by the S33 webhook
--                                deliverer; kept separate from the
--                                generic job table so S33 controls its
--                                own delivery cadence.
--   * repos.default_branch_oid — set by push:process the first time the
--                                repo's default branch receives a
--                                commit; the home page reads it to
--                                decide whether to render the empty or
--                                populated view (S11/S17).
--
-- Why a Postgres queue instead of Redis: keeps the runtime dependency
-- surface to one engine; FOR UPDATE SKIP LOCKED gives us safe concurrent
-- dispatch out of the box; LISTEN/NOTIFY gives us idle wake-ups without
-- polling. If we ever need cross-region or millions-of-jobs throughput
-- we'll revisit, but that's well past MVP.

-- +goose Up

CREATE TABLE jobs (
    id              bigserial   PRIMARY KEY,
    kind            text        NOT NULL,
    payload         jsonb       NOT NULL DEFAULT '{}',
    run_at          timestamptz NOT NULL DEFAULT now(),
    attempts        int         NOT NULL DEFAULT 0,
    max_attempts    int         NOT NULL DEFAULT 5,
    last_error      text,
    locked_by       text,
    locked_at       timestamptz,
    completed_at    timestamptz,
    failed_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT jobs_kind_length        CHECK (char_length(kind) BETWEEN 1 AND 100),
    CONSTRAINT jobs_attempts_nonneg    CHECK (attempts >= 0),
    CONSTRAINT jobs_max_attempts_pos   CHECK (max_attempts > 0)
);

-- The dispatch index: workers query for the oldest runnable row of a given
-- kind. Partial because only un-completed and un-failed rows are dispatchable.
CREATE INDEX jobs_dispatch_idx
    ON jobs (kind, run_at)
    WHERE completed_at IS NULL AND failed_at IS NULL;

-- Visibility: which jobs are currently held by which worker.
CREATE INDEX jobs_locked_idx
    ON jobs (locked_by, locked_at)
    WHERE locked_by IS NOT NULL;


CREATE TABLE push_events (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    pusher_user_id  bigint      REFERENCES users(id) ON DELETE SET NULL,
    before_sha      text        NOT NULL,
    after_sha       text        NOT NULL,
    ref             text        NOT NULL,
    protocol        text        NOT NULL,
    request_id      text        NOT NULL DEFAULT '',
    processed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT push_events_protocol      CHECK (protocol IN ('http', 'ssh')),
    CONSTRAINT push_events_ref_length    CHECK (char_length(ref) BETWEEN 1 AND 255),
    CONSTRAINT push_events_sha_length    CHECK (
        char_length(before_sha) BETWEEN 1 AND 64 AND
        char_length(after_sha)  BETWEEN 1 AND 64
    )
);

CREATE INDEX push_events_repo_id_idx ON push_events (repo_id, created_at DESC);
CREATE INDEX push_events_unprocessed_idx
    ON push_events (created_at)
    WHERE processed_at IS NULL;


CREATE TABLE webhook_events_pending (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    event_kind      text        NOT NULL,
    payload         jsonb       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT webhook_events_pending_kind_length CHECK (char_length(event_kind) BETWEEN 1 AND 100)
);

CREATE INDEX webhook_events_pending_repo_id_idx
    ON webhook_events_pending (repo_id, created_at);


-- default_branch_oid is the OID at HEAD of repos.default_branch. NULL
-- until the first push lands; the repo home view checks this to fork
-- between empty and populated layouts (refs-on-disk fallback in S11).
ALTER TABLE repos ADD COLUMN default_branch_oid text;

-- +goose Down
ALTER TABLE repos DROP COLUMN IF EXISTS default_branch_oid;
DROP TABLE IF EXISTS webhook_events_pending;
DROP TABLE IF EXISTS push_events;
DROP TABLE IF EXISTS jobs;
