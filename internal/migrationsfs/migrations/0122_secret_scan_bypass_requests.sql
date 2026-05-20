-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP26b: push-protection bypass requests. These rows are metadata only:
-- pattern, path, line number, and commit OID. They intentionally do not
-- store raw matched bytes or redacted excerpts. A bypass is narrower
-- than the allowlist: approval suppresses exactly one
-- (repo, pattern, path, commit, line) finding until its expiry.

-- +goose Up
CREATE TYPE secret_scan_bypass_status AS ENUM ('pending', 'approved', 'denied');

CREATE TABLE secret_scan_bypass_requests (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    pattern         text        NOT NULL,
    path            text        NOT NULL,
    commit_oid      text        NOT NULL,
    line_no         int         NOT NULL,
    status          secret_scan_bypass_status NOT NULL DEFAULT 'pending',
    requested_by    bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    reviewed_by     bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    request_reason  text        NOT NULL DEFAULT '',
    review_note     text        NOT NULL DEFAULT '',
    approved_until  timestamptz NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    reviewed_at     timestamptz NULL,
    last_seen_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT secret_scan_bypass_requests_pattern_shape CHECK (
        char_length(pattern) BETWEEN 1 AND 80
    ),
    CONSTRAINT secret_scan_bypass_requests_path_shape CHECK (
        char_length(path) BETWEEN 1 AND 1024
    ),
    CONSTRAINT secret_scan_bypass_requests_commit_shape CHECK (
        char_length(commit_oid) BETWEEN 40 AND 64
    ),
    CONSTRAINT secret_scan_bypass_requests_line_no_positive CHECK (
        line_no >= 1
    ),
    CONSTRAINT secret_scan_bypass_requests_request_reason_shape CHECK (
        char_length(request_reason) BETWEEN 0 AND 500
    ),
    CONSTRAINT secret_scan_bypass_requests_review_note_shape CHECK (
        char_length(review_note) BETWEEN 0 AND 500
    ),
    CONSTRAINT secret_scan_bypass_requests_unique UNIQUE (
        repo_id, pattern, path, commit_oid, line_no
    ),
    CONSTRAINT secret_scan_bypass_requests_review_shape CHECK (
        (status = 'pending' AND reviewed_by IS NULL AND reviewed_at IS NULL AND approved_until IS NULL)
        OR (status = 'approved' AND reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL AND approved_until IS NOT NULL)
        OR (status = 'denied' AND reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL AND approved_until IS NULL)
    )
);

CREATE INDEX secret_scan_bypass_requests_repo_status_idx
    ON secret_scan_bypass_requests (repo_id, status, created_at DESC, id DESC);

CREATE INDEX secret_scan_bypass_requests_repo_approved_idx
    ON secret_scan_bypass_requests (repo_id, pattern, path, commit_oid, line_no)
    WHERE status = 'approved';

CREATE TRIGGER secret_scan_bypass_requests_set_updated_at
    BEFORE UPDATE ON secret_scan_bypass_requests
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS secret_scan_bypass_requests;
DROP TYPE IF EXISTS secret_scan_bypass_status;
