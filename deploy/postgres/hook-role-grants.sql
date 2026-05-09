-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Standalone hook-role grants. The Ansible postgres role applies
-- the same grants idempotently; this file exists so an operator
-- can re-apply (or audit) the exact write surface without running
-- the full playbook.
--
-- Contract: shithub_hook is the role assumed by `shithubd hook ...`
-- subprocesses (post-receive, pre-receive). It MUST NOT have any
-- access beyond what's listed here. If a hook subcommand needs a
-- new table, add it here in the same PR — grep `shithub_hook` in
-- cmd/shithubd/hook.go to confirm.
--
-- Apply as the shithub DB owner:
--   psql -U shithub -d shithub -f hook-role-grants.sql

BEGIN;

-- The role is created idempotently by the Ansible role; if you're
-- applying this by hand on a fresh DB, uncomment:
-- CREATE ROLE shithub_hook LOGIN PASSWORD :'hook_password';

-- Read surface: the hook needs to look up the pushing user, the
-- target repo, and the collaborator/permission rows to authorize
-- the push.
GRANT SELECT ON users               TO shithub_hook;
GRANT SELECT ON repos               TO shithub_hook;
GRANT SELECT ON repo_collaborators  TO shithub_hook;
GRANT SELECT ON orgs                TO shithub_hook;
GRANT SELECT ON org_members         TO shithub_hook;

-- Write surface: every row the hook subcommand inserts. Nothing
-- here gets UPDATE or DELETE — those happen out-of-band through
-- the web app or worker.
GRANT INSERT ON push_events       TO shithub_hook;
GRANT INSERT ON jobs              TO shithub_hook;
GRANT INSERT ON domain_events     TO shithub_hook;
GRANT INSERT ON auth_audit_log    TO shithub_hook;

-- Sequences for the SERIAL/BIGSERIAL ids on the insert tables.
GRANT USAGE, SELECT ON SEQUENCE push_events_id_seq    TO shithub_hook;
GRANT USAGE, SELECT ON SEQUENCE jobs_id_seq           TO shithub_hook;
GRANT USAGE, SELECT ON SEQUENCE domain_events_id_seq  TO shithub_hook;
GRANT USAGE, SELECT ON SEQUENCE auth_audit_log_id_seq TO shithub_hook;

COMMIT;
