# Upgrade

Routine release deploys. The deploy is one binary swap + a systemd
restart; the only place upgrades get exciting is around DB migrations
and the occasional config schema change.

## Standard release

```sh
# from a clean checkout of the release tag
git fetch --tags
git checkout v<version>
make deploy-check ANSIBLE_INVENTORY=staging
make deploy ANSIBLE_INVENTORY=staging
# ... canary period ...
make deploy ANSIBLE_INVENTORY=production
```

`shithubd migrate up` runs as the web service's ExecStartPre, so
the binary that needs the new schema is also the one that applies
it. Order on each host: ExecStartPre runs migrations → web starts
on the new schema.

If a migration is long (>30s), call it out in the release notes
and time the deploy outside peak hours. The web service hangs in
"activating" until ExecStartPre finishes.

## Canary

We deploy to staging first, watch for 30 min in Grafana. Things to
look at:

- p95 latency on the top routes (`shithubd-overview` dashboard).
- DB call rate — a 10× jump usually means a regressed N+1.
- Job queue depth — a stuck migration reflects here.
- Error logs in Loki: `{service="shithubd"} |~ "panic|ERROR"`.

If anything looks off, **do not** promote to production. Rollback
on staging is cheap; rollback on production is loud.

## Major version (database)

If the release notes flag a major schema change:

1. Take a manual `pg_dump` immediately before the deploy:
   `sudo -u postgres /usr/local/bin/shithub-backup-daily`.
2. Confirm it landed in Spaces.
3. Deploy to staging, run `make restore-drill` against the
   *post-deploy* dump to confirm the new schema restores cleanly.
4. Then production.

## Config schema changes

When a release adds a required env var, the binary refuses to start
and complains in the journal. Update `deploy/ansible/roles/shithubd/
templates/web.env.j2` (and `worker.env.j2`), bump the inventory
vars, redeploy. There's no separate migration step for env files.
