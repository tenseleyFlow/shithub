-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29 follow-up: an in-toto statement is a byte-exact artifact — DSSE
-- envelopes and any future signature verification are over the exact bytes.
-- jsonb re-canonicalizes on write, so the statement the server normalized
-- (compact, insertion key order) was not the statement the download served,
-- and byte_count described the pre-insert bytes. json keeps the input text
-- verbatim and still rejects non-JSON. Nothing queries this column with
-- jsonb operators.

-- +goose Up
ALTER TABLE repo_artifact_attestations
    DROP CONSTRAINT repo_artifact_attestations_statement_object;

ALTER TABLE repo_artifact_attestations
    ALTER COLUMN statement TYPE json USING statement::text::json;

ALTER TABLE repo_artifact_attestations
    ADD CONSTRAINT repo_artifact_attestations_statement_object CHECK (json_typeof(statement) = 'object');

-- Existing rows hold jsonb-normalized text; resync their stale byte_count.
UPDATE repo_artifact_attestations
SET byte_count = octet_length(statement::text)
WHERE byte_count <> octet_length(statement::text);

-- +goose Down
ALTER TABLE repo_artifact_attestations
    DROP CONSTRAINT repo_artifact_attestations_statement_object;

ALTER TABLE repo_artifact_attestations
    ALTER COLUMN statement TYPE jsonb USING statement::jsonb;

ALTER TABLE repo_artifact_attestations
    ADD CONSTRAINT repo_artifact_attestations_statement_object CHECK (jsonb_typeof(statement) = 'object');
