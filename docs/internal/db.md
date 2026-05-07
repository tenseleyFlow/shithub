# Database conventions

This document is the authoritative reference for shithub's PostgreSQL schema
conventions. Every domain sprint (S05 onwards) follows these.

## Engine and tooling

- **PostgreSQL 16**. Production runs self-hosted on a dedicated block volume
  (S37). Local dev runs in `docker-compose` (`make dev-db`).
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
  is monotonically increasing.
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

- App: `pgxpool.Pool` per process. Default sizing 25 in prod, 10 in dev.
- Hooks (S07/S14): tiny pools (max 4) since they're short-lived and on the
  critical path of git operations.
- Tests: per-test pool with max 2 connections.

## Test harness

- `internal/testing/dbtest`: `dbtest.NewTestDB(t)` creates a fresh database
  cloned from a template (which has all migrations applied) per test, then
  drops it on `t.Cleanup`. Parallel-safe.
- Tests requiring DB access set `SHITHUB_TEST_DATABASE_URL` pointing at a
  Postgres server with permission to `CREATE DATABASE`.

## Operational

- `pg_stat_statements` extension is loaded by default in dev compose and
  prod (S37). Used by S36's perf pass.
- `archive_mode=on` + WAL shipping to Spaces (cross-region) in prod (S37).
- Daily logical backups via `pg_dump --format=custom`, restored weekly to
  validate the backup chain.
