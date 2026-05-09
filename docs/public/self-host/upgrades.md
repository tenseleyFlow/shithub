# Upgrades & migrations

Routine release deploys. Migrations apply automatically; the
only place upgrades get exciting is around long migrations and
the occasional config schema change.

## Standard release

```sh
git fetch --tags
git checkout v<version>
make deploy-check ANSIBLE_INVENTORY=staging
make deploy ANSIBLE_INVENTORY=staging
# ... canary period ...
make deploy ANSIBLE_INVENTORY=production
```

`shithubd migrate up` runs as the web service's
`ExecStartPre=`, so the binary that needs the new schema is also
the one that applies it. Order on each host: ExecStartPre runs
migrations → web starts on the new schema.

If a migration is long (>30s), the release notes call it out.
Schedule the deploy outside peak hours; the web service hangs in
"activating" until ExecStartPre finishes.

## Canary

Deploy to staging first. Watch for 30 min in Grafana. Things to
look at:

- p95 latency on top routes.
- DB call rate — a 10× jump usually means a regressed N+1.
- Job queue depth — a stuck migration reflects here.
- Error logs in Loki: `{service="shithubd"} |~ "panic|ERROR"`.

If anything looks off, **do not** promote. Rollback on staging
is cheap; rollback on production is loud.

## Major release (database)

If the release notes flag a major schema change:

1. Take a manual `pg_dump` immediately before the deploy:
   `sudo -u postgres /usr/local/bin/shithub-backup-daily`.
2. Confirm it landed in Spaces.
3. Deploy to staging, run `make restore-drill` against the
   *post-deploy* dump to confirm the new schema restores cleanly.
4. Then production.

## Config schema changes

When a release adds a required config key, the binary refuses to
start and complains in the journal. Update
`deploy/ansible/roles/shithubd/templates/web.env.j2` (and
`worker.env.j2`), bump the inventory vars, redeploy. There's no
separate migration step for env files.

## Rolling back

See [Rollback (in-repo runbook)](https://github.com/tenseleyFlow/shithub/blob/main/docs/internal/runbooks/rollback.md).

Three rollback shapes, in preference order:

1. **Schema-compatible rollback (best).** If the migration only
   *added* columns/tables that the old code ignores, the old code
   runs against the new schema fine. Roll the code back; leave
   schema alone. Most of our migrations are deliberately additive
   for this reason.
2. **Roll forward to a hotfix.** If the migration changed
   semantics that the old code can't tolerate, ship a hotfix on
   top of the new release rather than reversing the migration.
3. **Migration `down` + code rollback.** Last resort; some `down`s
   drop columns and *will* lose data.

```sh
# (3) only when (1) and (2) won't work
ssh web-01
sudo -u shithub /usr/local/bin/shithubd migrate down  # ONE step
git checkout v<previous>
make deploy ANSIBLE_INVENTORY=production
```
