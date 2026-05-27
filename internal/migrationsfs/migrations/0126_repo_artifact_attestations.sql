-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29: repository artifact attestations. These are in-toto/SLSA-style
-- JSON statements associated with repository artifacts or build outputs.

-- +goose Up
CREATE TABLE repo_artifact_attestations (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    subject_name    text        NOT NULL,
    subject_digest  text        NOT NULL,
    predicate_type  text        NOT NULL,
    statement       jsonb       NOT NULL,
    byte_count      bigint      NOT NULL,
    source_run_id   bigint      NULL REFERENCES workflow_runs(id) ON DELETE SET NULL,
    uploaded_by     bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_artifact_attestations_subject_name_shape CHECK (char_length(subject_name) BETWEEN 1 AND 512),
    CONSTRAINT repo_artifact_attestations_subject_digest_shape CHECK (char_length(subject_digest) BETWEEN 1 AND 255),
    CONSTRAINT repo_artifact_attestations_subject_digest_algorithm CHECK (subject_digest ~ '^[A-Za-z0-9._+-]+:[A-Fa-f0-9]{16,}$'),
    CONSTRAINT repo_artifact_attestations_predicate_type_shape CHECK (char_length(predicate_type) BETWEEN 1 AND 512),
    CONSTRAINT repo_artifact_attestations_statement_object CHECK (jsonb_typeof(statement) = 'object'),
    CONSTRAINT repo_artifact_attestations_byte_count_positive CHECK (byte_count > 0)
);

CREATE INDEX repo_artifact_attestations_repo_created_idx
    ON repo_artifact_attestations (repo_id, created_at DESC, id DESC);

CREATE INDEX repo_artifact_attestations_subject_idx
    ON repo_artifact_attestations (repo_id, subject_digest);

-- +goose Down
DROP TABLE IF EXISTS repo_artifact_attestations;
