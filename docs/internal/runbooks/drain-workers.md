# Drain workers for maintenance

The worker is graceful-shutdown-clean: SIGTERM finishes the
in-flight job and exits; queued jobs stay queued and a future
worker picks them up. This makes "drain for maintenance" a
near-no-op — but here's the procedure when you want to be
precise.

## Stop accepting new work but finish what's running

```sh
ssh worker-host
sudo systemctl stop shithubd-worker
```

The unit honors `Type=notify` + a 30-second `TimeoutStopSec`. The
worker:

1. Stops the next `FOR UPDATE SKIP LOCKED` query.
2. Finishes the currently-leased job (if any).
3. Exits 0.

Queued jobs stay in the `jobs` table with `state='queued'`. A
restarted worker (or a different worker on a different host)
will pick them up.

## Confirm drain

```sh
psql -d shithub -c "
  SELECT state, count(*) FROM jobs GROUP BY state;"
```

You're looking for `processing=0`. If it's > 0 a few seconds
after `systemctl stop`, find the rogue worker:

```sh
psql -d shithub -c "
  SELECT id, kind, leased_by, leased_at FROM jobs WHERE state='processing';"
```

`leased_by` is a `<host>:<pid>` string; track down the host and
process and stop it.

## Drain only one job kind

If you need to do schema work that affects (e.g.) only the
search-reindex job, you can pause that kind without stopping the
worker:

```sql
UPDATE jobs
   SET state='paused'
 WHERE state='queued'
   AND kind='search.reindex';
```

The worker's leasing query filters out `paused`. Resume by
flipping back to `queued`.

This is **not** the standard maintenance path; reach for a full
drain unless you have a reason to keep other kinds running.

## Hostile case: a stuck job

If a worker is leased on a job and the worker process is gone
(host crashed mid-execution), the lease times out and a new
worker re-leases the same job. The lease timeout is set per-kind
in the job handler registry; default 5 minutes.

To force re-lease sooner (e.g., the job is small and you want
it picked up now):

```sql
UPDATE jobs
   SET leased_at = NULL, leased_by = NULL, state = 'queued'
 WHERE id = <job-id>;
```

Audit-trail your manual intervention in the incident channel.

## Resume

```sh
sudo systemctl start shithubd-worker
```

The worker starts polling immediately; queued jobs begin running
within seconds.
