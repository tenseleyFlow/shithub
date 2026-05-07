-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Public profile fields. All optional. avatar_object_key references the
-- avatars/<owner>/<size>.<ext> prefix on object storage; NULL means
-- "render the deterministic identicon."

-- +goose Up
ALTER TABLE users
    ADD COLUMN bio               text NOT NULL DEFAULT '',
    ADD COLUMN location          text NOT NULL DEFAULT '',
    ADD COLUMN website           text NOT NULL DEFAULT '',
    ADD COLUMN company           text NOT NULL DEFAULT '',
    ADD COLUMN pronouns          text NOT NULL DEFAULT '',
    ADD COLUMN avatar_object_key text;

ALTER TABLE users
    ADD CONSTRAINT users_bio_length      CHECK (char_length(bio)      <= 500),
    ADD CONSTRAINT users_location_length CHECK (char_length(location) <= 80),
    ADD CONSTRAINT users_website_length  CHECK (char_length(website)  <= 200),
    ADD CONSTRAINT users_company_length  CHECK (char_length(company)  <= 80),
    ADD CONSTRAINT users_pronouns_length CHECK (char_length(pronouns) <= 40);

-- +goose Down
ALTER TABLE users
    DROP COLUMN IF EXISTS bio,
    DROP COLUMN IF EXISTS location,
    DROP COLUMN IF EXISTS website,
    DROP COLUMN IF EXISTS company,
    DROP COLUMN IF EXISTS pronouns,
    DROP COLUMN IF EXISTS avatar_object_key;
