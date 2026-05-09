-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Smoke checks for the restore drill. Each \echo line is the human
-- name of the check. We use psql's ON_ERROR_STOP=1 so any failed
-- assertion exits non-zero and the drill reports failure.
--
-- Keep these checks coarse: count tables, sanity-check primary
-- foreign-key relationships, ensure migrations table exists. Don't
-- compare counts to fixed numbers — counts grow over time. Compare
-- to internal invariants instead.

\echo === schema_migrations exists and has rows ===
SELECT 1 / (CASE WHEN COUNT(*) > 0 THEN 1 ELSE 0 END) AS ok
  FROM schema_migrations;

\echo === core tables non-empty ===
SELECT 1 / (CASE WHEN COUNT(*) > 0 THEN 1 ELSE 0 END) FROM users;
SELECT 1 / (CASE WHEN COUNT(*) > 0 THEN 1 ELSE 0 END) FROM repos;

\echo === every repo has a real owner ===
SELECT 1 / (CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END) AS orphan_repos
  FROM repos r
  LEFT JOIN users u ON u.id = r.owner_id
  LEFT JOIN orgs  o ON o.id = r.owner_id
  WHERE u.id IS NULL AND o.id IS NULL;

\echo === every push_event references a real repo ===
SELECT 1 / (CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END) AS orphan_push_events
  FROM push_events pe
  LEFT JOIN repos r ON r.id = pe.repo_id
  WHERE r.id IS NULL;

\echo === every issue belongs to a repo ===
SELECT 1 / (CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END) AS orphan_issues
  FROM issues i
  LEFT JOIN repos r ON r.id = i.repo_id
  WHERE r.id IS NULL;

\echo === auth_audit_log columns intact ===
SELECT actor_user_id, action, occurred_at FROM auth_audit_log LIMIT 1;

\echo === migrations applied through latest known ===
SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1;
