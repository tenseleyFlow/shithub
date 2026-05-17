-- +goose Up
-- SPDX-License-Identifier: AGPL-3.0-or-later

CREATE TYPE repo_project_state AS ENUM ('open', 'closed');

CREATE TABLE repo_projects (
    id                 bigserial PRIMARY KEY,
    repo_id            bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    title              text NOT NULL,
    description        text NOT NULL DEFAULT '',
    state              repo_project_state NOT NULL DEFAULT 'open',
    created_by_user_id bigint REFERENCES users(id) ON DELETE SET NULL,
    closed_at          timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_projects_title_length CHECK (char_length(title) BETWEEN 1 AND 200),
    CONSTRAINT repo_projects_description_length CHECK (char_length(description) <= 2000)
);

CREATE INDEX repo_projects_repo_state_idx
    ON repo_projects(repo_id, state, updated_at DESC, id DESC);

CREATE TRIGGER tg_repo_projects_updated_at
    BEFORE UPDATE ON repo_projects
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE repo_project_items (
    id                 bigserial PRIMARY KEY,
    project_id         bigint NOT NULL REFERENCES repo_projects(id) ON DELETE CASCADE,
    issue_id           bigint NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    added_by_user_id   bigint REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_project_items_unique_issue UNIQUE (project_id, issue_id)
);

CREATE INDEX repo_project_items_issue_idx
    ON repo_project_items(issue_id, created_at DESC);

CREATE TABLE repo_wiki_pages (
    id                 bigserial PRIMARY KEY,
    repo_id            bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    slug               text NOT NULL,
    title              text NOT NULL,
    body               text NOT NULL DEFAULT '',
    body_html_cached   text,
    created_by_user_id bigint REFERENCES users(id) ON DELETE SET NULL,
    updated_by_user_id bigint REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_wiki_pages_slug_length CHECK (char_length(slug) BETWEEN 1 AND 120),
    CONSTRAINT repo_wiki_pages_slug_shape CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$'),
    CONSTRAINT repo_wiki_pages_title_length CHECK (char_length(title) BETWEEN 1 AND 200),
    CONSTRAINT repo_wiki_pages_body_length CHECK (char_length(body) <= 262144),
    CONSTRAINT repo_wiki_pages_repo_slug_unique UNIQUE (repo_id, slug)
);

CREATE INDEX repo_wiki_pages_repo_updated_idx
    ON repo_wiki_pages(repo_id, updated_at DESC, id DESC);

CREATE TRIGGER tg_repo_wiki_pages_updated_at
    BEFORE UPDATE ON repo_wiki_pages
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down

DROP TRIGGER IF EXISTS tg_repo_wiki_pages_updated_at ON repo_wiki_pages;
DROP TABLE IF EXISTS repo_wiki_pages;

DROP INDEX IF EXISTS repo_project_items_issue_idx;
DROP TABLE IF EXISTS repo_project_items;

DROP TRIGGER IF EXISTS tg_repo_projects_updated_at ON repo_projects;
DROP INDEX IF EXISTS repo_projects_repo_state_idx;
DROP TABLE IF EXISTS repo_projects;

DROP TYPE IF EXISTS repo_project_state;
