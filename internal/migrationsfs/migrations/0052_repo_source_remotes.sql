-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Durable public source remotes for repo imports and submodule gitlink
-- hydration. A repo may remember one upstream fetch URL; fetch status is
-- advisory so failed imports can be retried from settings without losing
-- the configured remote.

-- +goose Up

CREATE TABLE repo_source_remotes (
    repo_id bigint PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    remote_url text NOT NULL,
    last_fetched_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (remote_url <> ''),
    CHECK (length(remote_url) <= 2048)
);

CREATE TRIGGER repo_source_remotes_set_updated_at
BEFORE UPDATE ON repo_source_remotes
FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


-- +goose Down

DROP TRIGGER IF EXISTS repo_source_remotes_set_updated_at ON repo_source_remotes;
DROP TABLE IF EXISTS repo_source_remotes;
