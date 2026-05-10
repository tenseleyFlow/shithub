-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Smoke checks for the restore drill. Each \echo line is the human
-- name of the check. We use psql's ON_ERROR_STOP=1 so any failed
-- assertion exits non-zero and the drill reports failure.
--
-- Keep these checks coarse: count tables, sanity-check primary
-- foreign-key relationships, ensure the migration tracking table
-- exists. Don't compare counts to fixed numbers — counts grow over
-- time. Compare to internal invariants instead. Don't assert that
-- non-system tables are non-empty (that breaks day-one drills).

\echo === goose_db_version exists and has rows ===
SELECT 1 / (CASE WHEN COUNT(*) > 0 THEN 1 ELSE 0 END) AS ok
  FROM goose_db_version;

\echo === users table exists (the only table guaranteed non-empty) ===
SELECT 1 / (CASE WHEN COUNT(*) > 0 THEN 1 ELSE 0 END) FROM users;

\echo === repos table exists (may legitimately be empty on a fresh instance) ===
SELECT COUNT(*) FROM repos;

\echo === every repo has a real owner ===
-- A repo's owner is exactly one of {owner_user_id, owner_org_id}; the
-- other is NULL. Both being NULL or both being non-NULL is corruption.
SELECT 1 / (CASE WHEN COUNT(*) = 0 THEN 1 ELSE 0 END) AS orphan_repos
  FROM repos r
  LEFT JOIN users u ON u.id = r.owner_user_id
  LEFT JOIN orgs  o ON o.id = r.owner_org_id
  WHERE (r.owner_user_id IS NULL AND r.owner_org_id IS NULL)
     OR (r.owner_user_id IS NOT NULL AND u.id IS NULL)
     OR (r.owner_org_id IS NOT NULL AND o.id IS NULL);

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
SELECT actor_id, action, target_type, created_at FROM auth_audit_log LIMIT 1;

\echo === migrations applied through latest known ===
SELECT version_id FROM goose_db_version ORDER BY id DESC LIMIT 1;
