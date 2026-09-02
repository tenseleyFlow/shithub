-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29 follow-up: SBOM downloads must be byte-stable and byte_count must
-- describe the bytes we serve. jsonb re-canonicalizes on write — keys sorted
-- by (length, bytes), whitespace dropped, ": " separators re-emitted — so the
-- pretty-printed SPDX document the generator produced was never the document
-- that came back out, and byte_count (counted before the insert) was always
-- wrong. json stores the input text verbatim and still rejects non-JSON.
-- Nothing queries these columns with jsonb operators.

-- +goose Up
ALTER TABLE repo_sbom_exports
    DROP CONSTRAINT repo_sbom_exports_document_object;

ALTER TABLE repo_sbom_exports
    ALTER COLUMN document TYPE json USING document::text::json;

ALTER TABLE repo_sbom_exports
    ADD CONSTRAINT repo_sbom_exports_document_object CHECK (json_typeof(document) = 'object');

-- Existing rows hold jsonb-normalized text; resync their stale byte_count.
UPDATE repo_sbom_exports
SET byte_count = octet_length(document::text)
WHERE byte_count <> octet_length(document::text);

-- +goose Down
ALTER TABLE repo_sbom_exports
    DROP CONSTRAINT repo_sbom_exports_document_object;

ALTER TABLE repo_sbom_exports
    ALTER COLUMN document TYPE jsonb USING document::jsonb;

ALTER TABLE repo_sbom_exports
    ADD CONSTRAINT repo_sbom_exports_document_object CHECK (jsonb_typeof(document) = 'object');
