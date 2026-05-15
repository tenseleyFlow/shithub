-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-10b: secret-scan findings storage. The worker walks the
-- repo's default-branch tree, runs the curated pattern engine
-- (internal/secretscan, shipped in 10a) over each text blob, and
-- writes one row per match.
--
-- CRITICAL: the raw matched bytes are NEVER stored. Only the pattern
-- name + path + line + a *redacted* excerpt land in the DB. This is
-- the same invariant the scan engine guarantees in-process; the
-- migration constraints + the worker code enforce it at rest.
--
-- Status enum supports the lifecycle:
--   open         — fresh finding, unresolved
--   resolved     — the user verified + remediated; informational
--   allowlisted  — user marked as false-positive via the (path, pattern)
--                  allowlist that 10c ships
--   stale        — re-scan found the matched line removed from HEAD;
--                  worker auto-marks rather than deletes so the audit
--                  trail survives

-- +goose Up
CREATE TYPE secret_scan_finding_status AS ENUM ('open', 'resolved', 'allowlisted', 'stale');

CREATE TABLE secret_scan_findings (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    -- Pattern is the secretscan.Pattern.Name value. Stable text rather
    -- than an enum so adding new patterns doesn't require a migration.
    pattern         text        NOT NULL,
    path            text        NOT NULL,
    line_no         int         NOT NULL,
    -- Redacted excerpt: matched bytes replaced with [REDACTED] by the
    -- scan engine. Capped at 200 chars in the worker before insert.
    excerpt         text        NOT NULL,
    status          secret_scan_finding_status NOT NULL DEFAULT 'open',
    -- Commit OID of the default branch at the time of the scan that
    -- produced this finding. Lets the re-scan logic distinguish "still
    -- present" from "the line was removed since I scanned".
    first_seen_oid  text        NOT NULL,
    last_seen_oid   text        NOT NULL,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz NULL,
    resolution_note text        NULL,

    CONSTRAINT secret_scan_findings_pattern_shape CHECK (
        char_length(pattern) BETWEEN 1 AND 80
    ),
    CONSTRAINT secret_scan_findings_path_shape CHECK (
        char_length(path) BETWEEN 1 AND 1024
    ),
    CONSTRAINT secret_scan_findings_excerpt_shape CHECK (
        char_length(excerpt) BETWEEN 0 AND 400
    ),
    -- One open finding per (repo, pattern, path, line). A re-scan that
    -- finds the same secret on the same line updates last_seen_oid +
    -- last_seen_at via ON CONFLICT rather than inserting a duplicate.
    -- ALL statuses share the constraint so a re-scan that finds a
    -- previously-allowlisted line skips the insert silently.
    CONSTRAINT secret_scan_findings_unique UNIQUE (repo_id, pattern, path, line_no)
);

CREATE INDEX secret_scan_findings_repo_status_idx
    ON secret_scan_findings (repo_id, status, last_seen_at DESC);

CREATE INDEX secret_scan_findings_repo_path_idx
    ON secret_scan_findings (repo_id, path);

-- +goose Down
DROP TABLE IF EXISTS secret_scan_findings;
DROP TYPE IF EXISTS secret_scan_finding_status;
