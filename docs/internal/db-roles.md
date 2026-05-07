# Postgres roles

shithub uses a single Postgres role today (`shithub`) for everything —
the web server, the SSH dispatcher, the worker, and the git hooks all
connect with the same credentials. That's deliberate for development
ergonomics; production hardening (S37) splits roles along blast-radius
lines so an exploit in one surface can't read or mutate the rest of
the database.

This doc captures the **target end state** so S37 doesn't have to
re-derive it. None of these roles exist yet in dev or CI.

## Role plan

| Role             | Used by                              | Grants                                                                                                  |
| ---------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------- |
| `shithub`        | web server, worker, admin tooling    | Full owner of all tables (current behavior)                                                             |
| `shithub_hook`   | `shithubd hook pre-receive` / `post-receive` (S14) | `SELECT` on `users`, `repos`. `INSERT` on `push_events`, `jobs`, `webhook_events_pending`. Nothing else. |
| `shithub_akc`    | `shithubd ssh-authkeys` (S07/S13)    | `SELECT` on `user_ssh_keys`, `users`. `UPDATE` on `user_ssh_keys.last_used_at` (and that column only).  |
| `shithub_ro`     | future read-only replica or analytics | `SELECT` on every table, no write access at all                                                         |

The hook split is the highest-value because hooks run inside the push
process — fewer guardrails between a compromised git push and DB writes
than the web layer (which goes through CSRF + middleware + handlers).
The AKC split mirrors the contract from S07: that path only needs to
authenticate keys; if it can't write to repos or push_events the blast
radius is "log spam at worst."

## SQL recipe (S37 will turn this into a migration or Ansible task)

```sql
-- ─── shithub_hook ───────────────────────────────────────────────
CREATE ROLE shithub_hook LOGIN PASSWORD :'shithub_hook_password';

-- Reads (re-checked from DB so env staleness can't authorize a push):
GRANT SELECT (id, suspended_at, deleted_at)         ON users  TO shithub_hook;
GRANT SELECT (id, is_archived, deleted_at, default_branch) ON repos TO shithub_hook;

-- Writes (post-receive's exact INSERT surface):
GRANT INSERT                                  ON push_events           TO shithub_hook;
GRANT INSERT, SELECT, USAGE                   ON jobs                  TO shithub_hook;
GRANT INSERT                                  ON webhook_events_pending TO shithub_hook;
GRANT USAGE, SELECT                           ON SEQUENCE push_events_id_seq           TO shithub_hook;
GRANT USAGE, SELECT                           ON SEQUENCE jobs_id_seq                  TO shithub_hook;
GRANT USAGE, SELECT                           ON SEQUENCE webhook_events_pending_id_seq TO shithub_hook;

-- Connect & schema:
GRANT CONNECT ON DATABASE shithub TO shithub_hook;
GRANT USAGE   ON SCHEMA   public  TO shithub_hook;

-- Notify channel: pg_notify needs no extra grant (it's a function call).

-- Explicitly REVOKE the public defaults if any were inherited:
REVOKE ALL ON ALL TABLES    IN SCHEMA public FROM shithub_hook;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM shithub_hook;
-- Then re-apply the precise grants above. (Order: REVOKE first when
-- migrating an existing role; on a fresh CREATE ROLE the explicit
-- grants are already authoritative.)


-- ─── shithub_akc ────────────────────────────────────────────────
CREATE ROLE shithub_akc LOGIN PASSWORD :'shithub_akc_password';

GRANT SELECT (id, fingerprint, public_key, user_id, expires_at, revoked_at)
                      ON user_ssh_keys TO shithub_akc;
GRANT UPDATE (last_used_at, last_used_ip)
                      ON user_ssh_keys TO shithub_akc;
GRANT SELECT (id, username, suspended_at, deleted_at)
                      ON users         TO shithub_akc;

GRANT CONNECT ON DATABASE shithub TO shithub_akc;
GRANT USAGE   ON SCHEMA   public  TO shithub_akc;
```

## Plumbing changes when S37 lands

* **Config**: add `SHITHUB_HOOK_DATABASE_URL` and `SHITHUB_AKC_DATABASE_URL`
  to `internal/infra/config/config.go`. Fall back to `SHITHUB_DATABASE_URL`
  when unset (dev keeps single-role).
* **Hook subcommands**: `cmd/shithubd/hook.go::loadHookCtx` checks
  `SHITHUB_HOOK_DATABASE_URL` first, falls back to the main URL.
* **AKC subcommand**: `cmd/shithubd/ssh.go::sshAuthkeysCmd` checks
  `SHITHUB_AKC_DATABASE_URL` first.
* **Ansible role**: `deploy/ansible/roles/postgres/tasks/main.yml` runs
  the SQL recipe above against a fresh DB; passwords come from sops or
  1Password.
* **Migration policy**: don't add the GRANT statements to the goose
  migrations directory — those run as the `shithub` super-role and
  changing them would re-grant on every dev re-up. Roles + grants are
  Ansible territory, not migration territory.

## Why we deferred

S14's deliverables include "Hook DB connection: small pool, distinct
credentials with the minimum-needed grants." The dev path (single role)
is what S14 actually shipped. Splitting it now adds friction to every
new schema migration (each new write target needs a grant tweak) while
the schema is still iterating fast — by S37 the schema has settled
enough that the grant surface is stable.

## Tracking

* This doc is the design.
* `.docs/sprints/S37-deployment-automation.md` references this doc
  under "Postgres" → "Hook DB role split" so the deferred work is on
  the sprint's deliverable list, not floating.
* The S14 sprint spec (`.docs/sprints/S14-push-processing-pipeline.md`)
  remains the authoritative description of what S14 *should* have done
  in an ideal world; this doc explains what got cut and where it landed.
