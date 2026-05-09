-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S28 code search index.
--
-- Two tables, both scoped to a repo's default branch (named in
-- `ref_name` so we don't lock in "default" semantics — the worker
-- can index a different ref later if we expand v1):
--
--   code_search_paths   — per-(repo, ref, path), tsvector on the
--                         path string. Always populated regardless
--                         of file size (cheap).
--   code_search_content — per-(repo, ref, path), tsvector on file
--                         contents AND a trigram column for camel-
--                         /snake-case identifier substring matches
--                         that the FTS tokenizer mangles. Skipped
--                         when the file is > 256 KiB or non-text.
--
-- Both tables are rewritten by the `repo:index_code` worker job in
-- a single tx (delete-then-insert) so readers never see a partial
-- index. The atomic-swap shape lives in the worker, not here.
--
-- `last_indexed_oid` on `repos` lets the reconciler detect drift
-- (default_branch_oid moved but last_indexed_oid didn't catch up).

-- +goose Up

ALTER TABLE repos
    ADD COLUMN last_indexed_oid text;

CREATE TABLE code_search_paths (
    repo_id  bigint   NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    ref_name text     NOT NULL,
    path     text     NOT NULL,
    tsv      tsvector NOT NULL,
    PRIMARY KEY (repo_id, ref_name, path)
);

CREATE INDEX code_search_paths_tsv_idx
    ON code_search_paths USING GIN (tsv);

CREATE INDEX code_search_paths_path_trgm_idx
    ON code_search_paths USING GIN (path gin_trgm_ops);

CREATE TABLE code_search_content (
    repo_id     bigint   NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    ref_name    text     NOT NULL,
    path        text     NOT NULL,
    content_tsv tsvector NOT NULL,
    content_trgm text    NOT NULL,
    PRIMARY KEY (repo_id, ref_name, path)
);

CREATE INDEX code_search_content_tsv_idx
    ON code_search_content USING GIN (content_tsv);

-- Trigram on content for substring + identifier matches. The
-- column carries the (truncated) raw text; pg_trgm builds the
-- index off it. Truncate to 64 KiB at the worker layer to keep
-- pg_trgm rows bounded.
CREATE INDEX code_search_content_trgm_idx
    ON code_search_content USING GIN (content_trgm gin_trgm_ops);

-- +goose Down
DROP TABLE IF EXISTS code_search_content;
DROP TABLE IF EXISTS code_search_paths;
ALTER TABLE repos DROP COLUMN IF EXISTS last_indexed_oid;
