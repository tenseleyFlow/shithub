-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Include the owning user/org handle in repo search documents and
-- keep those documents fresh when owner display fields change.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION repos_search_tsv(
    repo_name citext,
    repo_description text,
    repo_owner_user_id bigint,
    repo_owner_org_id bigint
) RETURNS tsvector
    LANGUAGE plpgsql AS $$
DECLARE
    owner_login text := '';
    owner_display text := '';
BEGIN
    IF repo_owner_user_id IS NOT NULL THEN
        SELECT username::text, display_name
          INTO owner_login, owner_display
          FROM users
         WHERE id = repo_owner_user_id;
    ELSIF repo_owner_org_id IS NOT NULL THEN
        SELECT slug::text, display_name
          INTO owner_login, owner_display
          FROM orgs
         WHERE id = repo_owner_org_id;
    END IF;

    RETURN
        setweight(to_tsvector('shithub_search', coalesce(repo_name::text, '')), 'A') ||
        setweight(to_tsvector('shithub_search', coalesce(owner_login, '')), 'A') ||
        setweight(to_tsvector('shithub_search', coalesce(repo_description, '')), 'B') ||
        setweight(to_tsvector('shithub_search', coalesce(owner_display, '')), 'C');
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_repos_search_upsert() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repos_search (repo_id, tsv) VALUES (
        NEW.id,
        repos_search_tsv(NEW.name, NEW.description, NEW.owner_user_id, NEW.owner_org_id)
    )
    ON CONFLICT (repo_id) DO UPDATE
        SET tsv = EXCLUDED.tsv;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS repos_search_upsert ON repos;
CREATE TRIGGER repos_search_upsert
    AFTER INSERT OR UPDATE OF name, description, owner_user_id, owner_org_id ON repos
    FOR EACH ROW EXECUTE FUNCTION tg_repos_search_upsert();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_repos_search_user_owner_update() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repos_search (repo_id, tsv)
    SELECT r.id, repos_search_tsv(r.name, r.description, r.owner_user_id, r.owner_org_id)
      FROM repos r
     WHERE r.owner_user_id = NEW.id
    ON CONFLICT (repo_id) DO UPDATE
        SET tsv = EXCLUDED.tsv;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER repos_search_user_owner_update
    AFTER UPDATE OF username, display_name ON users
    FOR EACH ROW
    WHEN (OLD.username IS DISTINCT FROM NEW.username OR OLD.display_name IS DISTINCT FROM NEW.display_name)
    EXECUTE FUNCTION tg_repos_search_user_owner_update();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_repos_search_org_owner_update() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repos_search (repo_id, tsv)
    SELECT r.id, repos_search_tsv(r.name, r.description, r.owner_user_id, r.owner_org_id)
      FROM repos r
     WHERE r.owner_org_id = NEW.id
    ON CONFLICT (repo_id) DO UPDATE
        SET tsv = EXCLUDED.tsv;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER repos_search_org_owner_update
    AFTER UPDATE OF slug, display_name ON orgs
    FOR EACH ROW
    WHEN (OLD.slug IS DISTINCT FROM NEW.slug OR OLD.display_name IS DISTINCT FROM NEW.display_name)
    EXECUTE FUNCTION tg_repos_search_org_owner_update();

INSERT INTO repos_search (repo_id, tsv)
SELECT r.id, repos_search_tsv(r.name, r.description, r.owner_user_id, r.owner_org_id)
  FROM repos r
ON CONFLICT (repo_id) DO UPDATE
    SET tsv = EXCLUDED.tsv;

-- +goose Down

DROP TRIGGER IF EXISTS repos_search_org_owner_update ON orgs;
DROP FUNCTION IF EXISTS tg_repos_search_org_owner_update();
DROP TRIGGER IF EXISTS repos_search_user_owner_update ON users;
DROP FUNCTION IF EXISTS tg_repos_search_user_owner_update();

DROP TRIGGER IF EXISTS repos_search_upsert ON repos;
DROP FUNCTION IF EXISTS tg_repos_search_upsert();
DROP FUNCTION IF EXISTS repos_search_tsv(citext, text, bigint, bigint);

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

INSERT INTO repos_search (repo_id, tsv)
SELECT id,
       setweight(to_tsvector('shithub_search', coalesce(name::text, '')), 'A') ||
       setweight(to_tsvector('shithub_search', coalesce(description, '')), 'B')
  FROM repos
ON CONFLICT (repo_id) DO UPDATE
    SET tsv = EXCLUDED.tsv;
