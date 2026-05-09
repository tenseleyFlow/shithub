# Capacity planning

Rule-of-thumb numbers from the S36 bench harness on the reference
hardware (2-vCPU droplets, Postgres 16 on dedicated host).
Treat these as starting points; your traffic patterns will
differ.

## Single web host (2 vCPU / 4 GB)

| Workload                              | Sustainable rate     |
|---------------------------------------|----------------------|
| Anonymous repo home page              | ~600 req/s, p95 < 80 ms |
| Authenticated dashboard render        | ~250 req/s, p95 < 200 ms |
| Diff render (PR with ~30 files)       | ~40 req/s, p95 < 600 ms |
| Issue list + filters                  | ~200 req/s, p95 < 250 ms |
| Code search (small corpus)            | ~30 req/s, p95 < 800 ms |

The first ceiling you hit is **CPU on diff/highlight rendering**.
A second web host doubles the budget linearly; the DB is rarely
the bottleneck for read-heavy traffic on this hardware.

## Postgres host (2 vCPU / 8 GB / 100 GB SSD)

| Workload         | Sustainable rate                     |
|------------------|--------------------------------------|
| Read queries     | ~5,000 calls/sec sustained           |
| Write queries    | ~500 calls/sec sustained             |
| Connections      | 100 concurrent (`pgxpool max=10` × 10 web procs) |

If the read rate goes much beyond ~5k/s, suspect an N+1 — see
the troubleshooting page.

## Worker host (2 vCPU / 4 GB)

The worker is bursty: idle most of the time, then chews through
the queue when something happens. A single worker handles:

- ~150 webhook deliveries/sec (most of the time spent in the
  outbound HTTP client).
- ~40 fan-out events/sec (notifications + activity recompute).

A second worker scales nearly linearly thanks to `FOR UPDATE
SKIP LOCKED`.

## Object store

Sized by your push volume, not request rate. Spaces handles the
PUT rate of every WAL segment + nightly dump comfortably; we've
never seen the bucket as the bottleneck.

Daily cost order: **WAL > daily dumps > LFS/attachments**. Most
WAL is recovered cheaply by lifecycle (30-day retention). If
your WAL costs run away, your `archive_timeout` is probably set
too low.

## Repo storage on disk

Plan for ~3× your raw data size on the bare-repo filesystem:
- 1× the actual git data
- 1× for `git gc` working set on large repos
- 1× headroom

Repos that hit ~5 GiB get noticeably slow on clone; consider
splitting or archiving.

## When to scale up

Trigger | Action
---|---
p95 latency on top routes climbing | Add a second web host first.
DB call rate consistently > 5k/s | Look for N+1 *before* upgrading the DB.
Job queue depth > 1k for hours | Add a second worker host.
Disk on db host > 70% | Plan growth (it doesn't shrink). Increase WAL retention only after.
Disk on web host > 70% | Audit largest repos; clean up archived if possible. Then grow.
Argon2 CPU pegged on signup spikes | The per-IP and per-/24 throttles are doing their job; argon2 is slow on purpose. Tune `argon2.time` only if every login is slow, not just signups.

## What we haven't measured

The S36 bench harness covers the read-heavy paths. Numbers we
don't yet have:

- Push throughput at scale (large concurrent pushes).
- Search corpus performance beyond ~10 GB of code.
- Webhook fan-out at high event-per-second rates.

If your deployment is going to push these limits, run the bench
against your own staging before launching.
