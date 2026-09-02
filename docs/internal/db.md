# Database conventions

This document is the authoritative reference for shithub's PostgreSQL schema
conventions. Every domain sprint (S05 onwards) follows these.

## Engine and tooling

- **PostgreSQL 16**. Production runs self-hosted **on the application
  droplet** — not a dedicated database host — with the data directory on
  an attached block volume (`/data/pgdata`) and the server listening on
  `localhost` only. It shares 2 vCPU and 3.9 GB with web, worker, cron,
  Caddy and Alloy; see `docs/internal/deploy.md`. Local dev runs in
  `docker-compose` (`make dev-db`).
- **Driver:** `pgx/v5` (`github.com/jackc/pgx/v5`). Native `*pgxpool.Pool` for
  app code; the `stdlib` adapter is reserved for libraries that demand
  `*sql.DB`.
- **Migrations:** `goose` (`github.com/pressly/goose/v3`), used as a library
  via `shithubd migrate`. Plain SQL up/down blocks; one file per migration.
- **Code generation:** `sqlc`. Queries live under
  `internal/<domain>/queries/*.sql`; generated Go lives under
  `internal/<domain>/sqlc/`.

## Naming

| Concern | Convention |
|---|---|
| Tables | snake_case, plural (`users`, `repos`, `pull_requests`) |
| Columns | snake_case (`created_at`, `owner_user_id`) |
| Primary keys | `id` (column) |
| Indexes | `<table>_<columns>_idx` |
| Unique indexes / constraints | `<table>_<columns>_key` |
| Foreign keys | `<table>_<col>_fkey` (Postgres default) |
| Enums | `snake_case_enum` (e.g. `repo_visibility`) |
| Trigger functions | `tg_<purpose>` (e.g. `tg_set_updated_at`) |

## ID strategy

- **`bigserial id PRIMARY KEY`** for internal IDs. Compact, ordered, cheap.
- **UUID v7** for any ID exposed publicly via URL. Gives sortability without
  leaking tenant size. Stored as the canonical Postgres `uuid` type.
- **Per-repo issue/PR numbers** are NEITHER `bigserial` NOR UUID — they are
  per-`(repo_id)` monotonic counters maintained by a small counter table
  (S21).

## Timestamps

- Always **`timestamptz`**. Never `timestamp` (without timezone) — that's a
  foot-gun.
- `created_at timestamptz NOT NULL DEFAULT now()` on every table.
- `updated_at timestamptz NOT NULL DEFAULT now()` on every table that
  receives updates, with a `BEFORE UPDATE` trigger that re-stamps it:

  ```sql
  CREATE TRIGGER set_updated_at BEFORE UPDATE ON <table>
      FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();
  ```

  The `tg_set_updated_at()` function is created in `0001_meta.sql` and
  reused across every table.

## Foreign keys

- **Always specify `ON DELETE` explicitly.** `RESTRICT` (the default) is the
  safe choice; only use `CASCADE` when the lifecycle of the child IS the
  lifecycle of the parent.
- Index every FK column unless the child is naturally indexed by a covering
  composite index that starts with the FK.

## Soft deletes

- **No soft deletes by default.** Hard delete + audit-log row when audit
  matters.
- Exceptions exist where grace windows are required (user delete, repo
  delete, org delete) — these are explicit and documented per sprint.

## Migrations

- **Forward-only after production deploy.** `Down` blocks exist for dev
  convenience but are immutable post-deploy. Corrections come as new
  migrations.
- One change per migration. Filename: `NNNN_short_purpose.sql` where `NNNN`
  is monotonically increasing and globally unique. `scripts/lint-migration-versions.sh`
  enforces this in CI because goose panics on duplicate numeric versions before
  it can run any migration.
- Goose markers:

  ```
  -- +goose Up
  ...
  -- +goose Down
  ...
  ```

  Multi-statement DDL that needs PL/pgSQL must be wrapped in
  `-- +goose StatementBegin` / `-- +goose StatementEnd`.

## citext

Use `citext` (case-insensitive text) for identifiers that should match
case-insensitively but display case-preserved (`users.username`,
`user_emails.email`, `orgs.slug`, etc.). The extension is enabled in
`0001_meta.sql` (post-S05) — for now, S01 enables only the function we need.

## JSONB

- Acceptable for genuinely polymorphic data (event metadata, audit log
  details, notification summaries).
- Not a substitute for proper schema. If the shape is known and stable,
  model it as columns or a child table.

## Connection pooling

- Web: one `pgxpool.Pool`, `db.max_conns` (default **10**, see
  `internal/infra/config` and `internal/infra/db`).
- Worker: `resolveWorkerCount()` + 2 — **6** at the shipped
  `SHITHUB_WORKERS=4`. The pool is sized off the *resolved* count, not
  the raw flag; getting that wrong once put four workers on a two-conn
  pool (2026-09-02 sitrep, cause #9).
- Hooks, `ssh`, `cron` and `admin` subcommands: short-lived pools of
  2–4.
- Tests: per-test pool with max 2 connections.

Server-side `max_connections` in `postgresql.conf.j2` is **60**, which
covers 10 + 6 plus concurrent short-lived invocations with room to
spare. Raising a client pool means re-checking that number: on a
shared box each backend can claim `work_mem` per sort node, so
connection count is a memory decision, not just a concurrency one.

## Test harness

- `internal/testing/dbtest`: `dbtest.NewTestDB(t)` creates a fresh database
  cloned from a template (which has all migrations applied) per test, then
  drops it on `t.Cleanup`. Parallel-safe.
- Tests requiring DB access set `SHITHUB_TEST_DATABASE_URL` pointing at a
  Postgres server with permission to `CREATE DATABASE`.

## Operational

- `pg_stat_statements` is loaded by default in dev compose. It is
  **not** installed in production — see below.
- `archive_mode=on` + WAL shipping to Spaces (cross-region) in prod.
- Daily logical backups via `pg_dump --format=custom`, restored weekly to
  validate the backup chain (`runbooks/backups.md`).

## The Ansible postgresql.conf has never been applied

`deploy/ansible/roles/postgres/templates/postgresql.conf.j2` is
tracked, tuned, and **not on the box.** `shithub-app` runs Debian
package defaults:

| Setting | Live box | Template |
|---|---|---|
| `shared_buffers` | 128MB | 256MB |
| `work_mem` | 4MB | 4MB |
| `effective_cache_size` | 4GB (default) | 1GB |
| `maintenance_work_mem` | 64MB (default) | 64MB |
| `max_connections` | 100 | 60 |
| `shared_preload_libraries` | *(empty)* | `pg_stat_statements` |

Consequences worth knowing: there is no `pg_stat_statements`, so
there is no query attribution when the box is under load; and
`max_connections=100` on a shared 3.9 GB box is a larger worst case
than anything the app will actually open.

`deploy/audit/check-droplet-drift.sh` tracks
`/etc/postgresql/16/main/postgresql.conf` so this stays visible.

### Applying it safely

**Do not do this with a blind `make deploy`.** The play would rewrite
the config and hand off to a handler mid-day; `shared_buffers`,
`max_connections` and `shared_preload_libraries` all need a **restart**
(not a reload), which drops every open connection, and the deploy
pipeline restarts web and worker for unrelated reasons on every push
to trunk.

Do it deliberately, inside the **05:15–06:00 UTC quiet window** — after
`shithubd-cron.timer` at 05:15 and before the AIDE check at 06:00, with
the 03:17 `pg_dump` and the 01/07/13/19:23 rclone sync all clear:

```sh
# 1. Snapshot what is live, so a rollback is a file copy.
ssh root@shithub.sh "
  cp -a /etc/postgresql/16/main/postgresql.conf \
        /etc/postgresql/16/main/postgresql.conf.pre-ansible
  sudo -u postgres psql -x -c \"SELECT name, setting, unit, context
      FROM pg_settings WHERE name IN
        ('shared_buffers','work_mem','effective_cache_size',
         'maintenance_work_mem','max_connections',
         'shared_preload_libraries')\"
"

# 2. Confirm there is enough free memory for the larger shared_buffers
#    right now (the delta is ~128 MB, but check, do not assume).
ssh root@shithub.sh 'free -m; systemctl show shithubd-web -p MemoryCurrent'

# 3. Apply the db role ONLY. --check first; read the diff.
ANSIBLE_INVENTORY=production ANSIBLE_TAGS=db make deploy-check
ANSIBLE_INVENTORY=production ANSIBLE_TAGS=db make deploy

# 4. Validate the file parses BEFORE bouncing the server. `-C` reads a
#    setting out of the on-disk config without starting a server; on
#    Debian, -D is the CONFIG dir, not pgdata.
ssh root@shithub.sh 'sudo -u postgres \
  /usr/lib/postgresql/16/bin/postgres -D /etc/postgresql/16/main \
  -C shared_buffers'

# 5. Restart (not reload) and watch it come back.
ssh root@shithub.sh '
  systemctl restart postgresql@16-main
  sleep 5
  systemctl is-active postgresql@16-main
  sudo -u postgres psql -Atc "SHOW shared_buffers"
  sudo -u postgres psql -Atc "SHOW max_connections"
  journalctl -u postgresql@16-main -n 50 --no-pager
'

# 6. Create the extension (the preload alone does nothing).
ssh root@shithub.sh 'sudo -u postgres psql -d shithub \
  -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"'

# 7. Web and worker dropped their pools on the restart; confirm they
#    reconnected rather than wedging.
ssh root@shithub.sh '
  curl -fsS 127.0.0.1:8080/readyz
  journalctl -u shithubd-web -u shithubd-worker -n 50 --no-pager
'
```

Rollback is `cp` the `.pre-ansible` file back and restart again.

If Postgres refuses to start after the restart, the usual cause is
`shared_buffers` exceeding what the kernel will give it: lower it,
restart, and check `journalctl` for the shared-memory error before
trying anything else. Never delete `pgdata`.
