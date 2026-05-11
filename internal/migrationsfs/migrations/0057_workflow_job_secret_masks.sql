-- SPDX-License-Identifier: AGPL-3.0-or-later

-- +goose Up
CREATE TABLE workflow_job_secret_masks (
    job_id bigint PRIMARY KEY REFERENCES workflow_jobs(id) ON DELETE CASCADE,
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT workflow_job_secret_masks_nonce_length
        CHECK (octet_length(nonce) = 12),
    CONSTRAINT workflow_job_secret_masks_ciphertext_nonempty
        CHECK (octet_length(ciphertext) > 0)
);

-- +goose Down
DROP TABLE IF EXISTS workflow_job_secret_masks;
