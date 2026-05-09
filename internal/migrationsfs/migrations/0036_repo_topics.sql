-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S32 — Repo topics. One row per (repo, topic). Topic is a citext
-- so case-insensitive uniqueness comes free, and the shape is
-- compatible with case-insensitive search later. Caps:
--   - 20 topics per repo (enforced in the orchestrator)
--   - 50 chars per topic (CHECK below)
-- Topic shape (lowercase alphanumeric + hyphen, can't lead/trail with
-- hyphen) is enforced in the Go validator since regex CHECKs in
-- Postgres are noisier than the Go-side equivalent.

-- +goose Up
CREATE TABLE repo_topics (
    repo_id    bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    topic      citext      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (repo_id, topic),
    CONSTRAINT repo_topics_topic_length CHECK (char_length(topic::text) BETWEEN 1 AND 50)
);

-- Index for "repos with topic X" lookups (post-MVP topic-filter on
-- explore page). Cheap and indexes are eventually consistent with
-- the workload.
CREATE INDEX repo_topics_topic_idx ON repo_topics (topic);

-- +goose Down
DROP TABLE IF EXISTS repo_topics;
