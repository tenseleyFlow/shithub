-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Postgres extensions used by S28 search:
--   pg_trgm   — trigram similarity for code identifiers and substring
--               match where the FTS tokenizer breaks down (camelCase,
--               snake_case, mixed-language code).
--   unaccent  — strips Latin diacritics so "café" matches "cafe" in
--               human-name search.
--
-- Both ship with PostgreSQL contrib; no external server required.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- +goose Down
-- We don't drop the extensions on rollback — other migrations may
-- have started depending on them, and DROP EXTENSION cascades to
-- dependent objects. Leave them installed; the pure cost is one
-- catalog row each.
