-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S28 search index tables for repos, issues, users.
--
-- Each table is keyed 1:1 with its source row and holds a tsvector
-- maintained by AFTER triggers on the source. We use a separate
-- table rather than a generated column on the source so:
--   1. Existing query buckets don't have to change shape.
--   2. The tsv column doesn't bloat every row read in unrelated
--      contexts (e.g. the repo list page would otherwise pay the
--      cost of pulling the tsv on every paint).
--
-- The tsv config is `english` everywhere — good enough for v1.
-- Multi-language content is post-MVP (would need per-document
-- language detection and a config switch).
--
-- `unaccent` is composed via the dictionary chain so accent stripping
-- happens before stemming; the search-side tsquery uses the same
-- chain so "café" matches "cafe".
--
-- Code search lives in 0032 — splitting because it's the bulkier
-- of the two and ships with its own worker job.

-- +goose Up

-- Build a custom dictionary chain that runs `unaccent` first, then
-- english stemming. The same config is used by both index-side and
-- query-side calls so accents normalize consistently on both sides.
-- Lazy: skip if it already exists (e.g. on a re-up after partial
-- failure).
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = 'shithub_search') THEN
        CREATE TEXT SEARCH CONFIGURATION shithub_search (COPY = pg_catalog.english);
        ALTER TEXT SEARCH CONFIGURATION shithub_search
            ALTER MAPPING FOR hword, hword_part, word
            WITH unaccent, english_stem;
    END IF;
END $$;
-- +goose StatementEnd

-- ─── repos ─────────────────────────────────────────────────────────

CREATE TABLE repos_search (
    repo_id   bigint   PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    tsv       tsvector NOT NULL
);

CREATE INDEX repos_search_tsv_idx ON repos_search USING GIN (tsv);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_repos_search_upsert() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repos_search (repo_id, tsv) VALUES (
        NEW.id,
        setweight(to_tsvector('shithub_search', coalesce(NEW.name::text, '')), 'A') ||
        setweight(to_tsvector('shithub_search', coalesce(NEW.description, '')), 'B')
    )
    ON CONFLICT (repo_id) DO UPDATE
        SET tsv = EXCLUDED.tsv;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER repos_search_upsert
    AFTER INSERT OR UPDATE OF name, description ON repos
    FOR EACH ROW EXECUTE FUNCTION tg_repos_search_upsert();

-- Backfill any existing rows.
INSERT INTO repos_search (repo_id, tsv)
SELECT id,
       setweight(to_tsvector('shithub_search', coalesce(name::text, '')), 'A') ||
       setweight(to_tsvector('shithub_search', coalesce(description, '')), 'B')
FROM repos
ON CONFLICT (repo_id) DO NOTHING;

-- ─── issues ────────────────────────────────────────────────────────

CREATE TABLE issues_search (
    issue_id        bigint   PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    repo_id         bigint   NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    kind            issue_kind NOT NULL,
    state           issue_state NOT NULL,
    author_user_id  bigint   REFERENCES users(id) ON DELETE SET NULL,
    tsv             tsvector NOT NULL
);

CREATE INDEX issues_search_tsv_idx       ON issues_search USING GIN (tsv);
CREATE INDEX issues_search_repo_idx      ON issues_search (repo_id);
CREATE INDEX issues_search_state_idx     ON issues_search (state);
CREATE INDEX issues_search_author_idx    ON issues_search (author_user_id) WHERE author_user_id IS NOT NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_issues_search_upsert() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO issues_search (issue_id, repo_id, kind, state, author_user_id, tsv) VALUES (
        NEW.id, NEW.repo_id, NEW.kind, NEW.state, NEW.author_user_id,
        setweight(to_tsvector('shithub_search', coalesce(NEW.title, '')), 'A') ||
        setweight(to_tsvector('shithub_search', coalesce(NEW.body, '')), 'B')
    )
    ON CONFLICT (issue_id) DO UPDATE
        SET repo_id        = EXCLUDED.repo_id,
            kind           = EXCLUDED.kind,
            state          = EXCLUDED.state,
            author_user_id = EXCLUDED.author_user_id,
            tsv            = EXCLUDED.tsv;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER issues_search_upsert
    AFTER INSERT OR UPDATE OF title, body, state, kind ON issues
    FOR EACH ROW EXECUTE FUNCTION tg_issues_search_upsert();

INSERT INTO issues_search (issue_id, repo_id, kind, state, author_user_id, tsv)
SELECT id, repo_id, kind, state, author_user_id,
       setweight(to_tsvector('shithub_search', coalesce(title, '')), 'A') ||
       setweight(to_tsvector('shithub_search', coalesce(body, '')), 'B')
FROM issues
ON CONFLICT (issue_id) DO NOTHING;

-- ─── users ─────────────────────────────────────────────────────────

CREATE TABLE users_search (
    user_id bigint   PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    tsv     tsvector NOT NULL
);

CREATE INDEX users_search_tsv_idx ON users_search USING GIN (tsv);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_users_search_upsert() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO users_search (user_id, tsv) VALUES (
        NEW.id,
        setweight(to_tsvector('shithub_search', coalesce(NEW.username::text, '')), 'A') ||
        setweight(to_tsvector('shithub_search', coalesce(NEW.display_name, '')), 'B') ||
        setweight(to_tsvector('shithub_search', coalesce(NEW.bio, '')), 'C')
    )
    ON CONFLICT (user_id) DO UPDATE
        SET tsv = EXCLUDED.tsv;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER users_search_upsert
    AFTER INSERT OR UPDATE OF username, display_name, bio ON users
    FOR EACH ROW EXECUTE FUNCTION tg_users_search_upsert();

INSERT INTO users_search (user_id, tsv)
SELECT id,
       setweight(to_tsvector('shithub_search', coalesce(username::text, '')), 'A') ||
       setweight(to_tsvector('shithub_search', coalesce(display_name, '')), 'B') ||
       setweight(to_tsvector('shithub_search', coalesce(bio, '')), 'C')
FROM users
ON CONFLICT (user_id) DO NOTHING;

-- +goose Down
DROP TRIGGER IF EXISTS users_search_upsert ON users;
DROP FUNCTION IF EXISTS tg_users_search_upsert();
DROP TABLE IF EXISTS users_search;

DROP TRIGGER IF EXISTS issues_search_upsert ON issues;
DROP FUNCTION IF EXISTS tg_issues_search_upsert();
DROP TABLE IF EXISTS issues_search;

DROP TRIGGER IF EXISTS repos_search_upsert ON repos;
DROP FUNCTION IF EXISTS tg_repos_search_upsert();
DROP TABLE IF EXISTS repos_search;

DROP TEXT SEARCH CONFIGURATION IF EXISTS shithub_search;
