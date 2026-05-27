-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29: persisted repository SBOM exports. The document is derived from the
-- existing dependency snapshot and stored so downloads are stable until the
-- user regenerates after a new dependency scan.

-- +goose Up
CREATE TABLE repo_sbom_exports (
    repo_id                          bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    format                           text        NOT NULL,
    source_head_sha                  text        NOT NULL,
    dependency_snapshot_generated_at timestamptz NOT NULL,
    document                         jsonb       NOT NULL,
    byte_count                       bigint      NOT NULL,
    generated_by                     bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    generated_at                     timestamptz NOT NULL DEFAULT now(),
    updated_at                       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (repo_id, format),
    CONSTRAINT repo_sbom_exports_format_known CHECK (format IN ('spdx-json')),
    CONSTRAINT repo_sbom_exports_source_head_sha_shape CHECK (char_length(source_head_sha) BETWEEN 1 AND 128),
    CONSTRAINT repo_sbom_exports_document_object CHECK (jsonb_typeof(document) = 'object'),
    CONSTRAINT repo_sbom_exports_byte_count_positive CHECK (byte_count > 0)
);

CREATE INDEX repo_sbom_exports_generated_idx
    ON repo_sbom_exports (repo_id, generated_at DESC);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON repo_sbom_exports
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS repo_sbom_exports;
