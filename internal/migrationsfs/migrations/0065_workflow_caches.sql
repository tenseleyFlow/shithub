-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Workflow caches. Records of cached workflow artifacts uploaded by
-- the `actions/cache@v*` workflow step. Mirrors GitHub Actions'
-- cache semantics:
--
--   * cache_key: the user-supplied cache key (e.g. "node-modules-${{ hashFiles(...) }}").
--   * cache_version: a content-derived version string (matches gh's
--     `version` field — opaque to the server).
--   * git_ref: the ref the cache was created against. The actions/cache
--     restore logic looks up caches for the current ref first, then
--     falls back to the repo's default branch.
--   * object_key: location of the tarball in the shared object store.
--   * size_bytes: persisted size for capacity-tracking.
--
-- A row uniquely identifies a cache by (repo_id, cache_key,
-- cache_version, git_ref) — multiple refs/versions can share a key,
-- but a given (key, version, ref) is one tarball.
--
-- last_accessed_at is bumped by the runner on cache restore so the
-- LRU eviction policy can purge oldest entries first. The §13 REST
-- surface exposes list + delete; the eviction sweeper runs out of
-- band (future sprint).
--
-- The runner-side write path that POPULATES this table is not yet
-- implemented — the actions/cache toolkit's upload protocol is its
-- own sprint. This table + the REST surface land first so operators
-- have an observation seat for when caches arrive.

-- +goose Up
CREATE TABLE workflow_caches (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    cache_key       text        NOT NULL,
    cache_version   text        NOT NULL,
    git_ref         text        NOT NULL,
    object_key      text        NOT NULL,
    size_bytes      bigint      NOT NULL DEFAULT 0,
    last_accessed_at timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (repo_id, cache_key, cache_version, git_ref),
    CONSTRAINT workflow_caches_cache_key_length CHECK (char_length(cache_key) BETWEEN 1 AND 512),
    CONSTRAINT workflow_caches_cache_version_length CHECK (char_length(cache_version) BETWEEN 1 AND 256),
    CONSTRAINT workflow_caches_git_ref_length CHECK (char_length(git_ref) BETWEEN 1 AND 256),
    CONSTRAINT workflow_caches_size_nonneg CHECK (size_bytes >= 0)
);

CREATE INDEX workflow_caches_repo_id_idx
    ON workflow_caches (repo_id, last_accessed_at DESC);

CREATE INDEX workflow_caches_repo_id_key_idx
    ON workflow_caches (repo_id, cache_key);

-- +goose Down
DROP TABLE IF EXISTS workflow_caches;
