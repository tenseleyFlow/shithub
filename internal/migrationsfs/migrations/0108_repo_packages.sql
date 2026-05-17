-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS SP22 — repository packages and package storage accounting.
--
-- This is the first package-hosting surface: generic package blobs attached
-- to repositories, with visibility inherited from the repository and package
-- bytes included in organization storage quota accounting.

-- +goose Up

CREATE TABLE repo_packages (
    id                 bigserial   PRIMARY KEY,
    repo_id            bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    name               text        NOT NULL,
    normalized_name    text        GENERATED ALWAYS AS (lower(name)) STORED,
    package_type       text        NOT NULL DEFAULT 'generic',
    description        text        NOT NULL DEFAULT '',
    latest_version     text        NOT NULL DEFAULT '',
    package_bytes      bigint      NOT NULL DEFAULT 0,
    created_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    updated_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_packages_name_length CHECK (char_length(name) BETWEEN 1 AND 128),
    CONSTRAINT repo_packages_name_format CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*$'),
    CONSTRAINT repo_packages_type_supported CHECK (package_type IN ('generic')),
    CONSTRAINT repo_packages_description_length CHECK (char_length(description) <= 1000),
    CONSTRAINT repo_packages_latest_version_length CHECK (char_length(latest_version) <= 128),
    CONSTRAINT repo_packages_package_bytes_nonneg CHECK (package_bytes >= 0),
    CONSTRAINT repo_packages_repo_name_type_unique UNIQUE (repo_id, normalized_name, package_type)
);

CREATE INDEX repo_packages_repo_updated_idx
    ON repo_packages (repo_id, updated_at DESC, id DESC);

CREATE TRIGGER tg_repo_packages_updated_at
    BEFORE UPDATE ON repo_packages
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE repo_package_versions (
    id                 bigserial   PRIMARY KEY,
    package_id         bigint      NOT NULL REFERENCES repo_packages(id) ON DELETE CASCADE,
    version            text        NOT NULL,
    size_bytes         bigint      NOT NULL DEFAULT 0,
    created_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_package_versions_version_length CHECK (char_length(version) BETWEEN 1 AND 128),
    CONSTRAINT repo_package_versions_version_format CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._+-]*$'),
    CONSTRAINT repo_package_versions_size_nonneg CHECK (size_bytes >= 0),
    CONSTRAINT repo_package_versions_unique_version UNIQUE (package_id, version)
);

CREATE INDEX repo_package_versions_package_created_idx
    ON repo_package_versions (package_id, created_at DESC, id DESC);

CREATE TABLE repo_package_files (
    id                 bigserial   PRIMARY KEY,
    version_id         bigint      NOT NULL REFERENCES repo_package_versions(id) ON DELETE CASCADE,
    filename           text        NOT NULL,
    object_key         text        NOT NULL,
    content_type       text        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes         bigint      NOT NULL,
    etag               text        NOT NULL DEFAULT '',
    created_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_package_files_filename_length CHECK (char_length(filename) BETWEEN 1 AND 255),
    CONSTRAINT repo_package_files_filename_format CHECK (
        filename !~ '[/:]' AND filename <> '.' AND filename <> '..'
    ),
    CONSTRAINT repo_package_files_object_key_length CHECK (char_length(object_key) BETWEEN 1 AND 1024),
    CONSTRAINT repo_package_files_content_type_length CHECK (char_length(content_type) BETWEEN 1 AND 255),
    CONSTRAINT repo_package_files_size_nonneg CHECK (size_bytes >= 0),
    CONSTRAINT repo_package_files_etag_length CHECK (char_length(etag) <= 255),
    CONSTRAINT repo_package_files_object_key_unique UNIQUE (object_key),
    CONSTRAINT repo_package_files_version_filename_unique UNIQUE (version_id, filename)
);

CREATE INDEX repo_package_files_version_created_idx
    ON repo_package_files (version_id, created_at DESC, id DESC);

ALTER TABLE org_usage_counters
    ADD COLUMN package_storage_bytes bigint NOT NULL DEFAULT 0
        CONSTRAINT org_usage_counters_package_storage_nonneg CHECK (package_storage_bytes >= 0);

ALTER TABLE org_usage_snapshots
    ADD COLUMN package_storage_bytes bigint NOT NULL DEFAULT 0
        CONSTRAINT org_usage_snapshots_package_storage_nonneg CHECK (package_storage_bytes >= 0);

-- +goose Down

ALTER TABLE org_usage_snapshots
    DROP COLUMN IF EXISTS package_storage_bytes;

ALTER TABLE org_usage_counters
    DROP COLUMN IF EXISTS package_storage_bytes;

DROP INDEX IF EXISTS repo_package_files_version_created_idx;
DROP TABLE IF EXISTS repo_package_files;

DROP INDEX IF EXISTS repo_package_versions_package_created_idx;
DROP TABLE IF EXISTS repo_package_versions;

DROP TRIGGER IF EXISTS tg_repo_packages_updated_at ON repo_packages;
DROP INDEX IF EXISTS repo_packages_repo_updated_idx;
DROP TABLE IF EXISTS repo_packages;
