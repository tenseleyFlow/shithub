# Status

Live status of `shithub.example` and the published mirrors.
This page is hand-maintained by the operator on call; the
machine-readable health endpoints are linked under each section.

> A full statuspage.io-style dashboard is planned post-MVP. Until
> then, this page + the operator's incident notes are the
> authoritative status source.

## Right now

**All systems normal.**

Last updated: TBD (operator updates on each cutover or incident).

## Health endpoints

These return immediately and reflect the running web process:

- [`https://shithub.example/-/health`](https://shithub.example/-/health) —
  `200 OK` with version + commit + buildAt when the web service
  is up.
- [`https://shithub.example/healthz`](https://shithub.example/healthz) —
  liveness only; `200 OK` if the process is responding.
- [`https://shithub.example/readyz`](https://shithub.example/readyz) —
  readiness; `200 OK` only when DB + storage are reachable.

A scripted check from your machine:

```sh
curl -fsS https://shithub.example/readyz
```

Non-200 means the web service is degraded or down. The operator's
monitoring also alerts on this; you don't need to poll it.

## Subdomains

| Subdomain                  | Purpose                | Health                                      |
|----------------------------|------------------------|---------------------------------------------|
| `shithub.example`          | App                    | `/readyz`                                   |
| `docs.shithub.example`     | Docs site (this site)  | Static; HTTP 200 on `/`                    |

## Backups

The most recent successful backup is recorded by the operator's
monitoring. Quarterly restore drills are scheduled; the last one's
log is archived to the backup bucket
(`shithub-backups/drills/<YYYY-Q>/`).

## Mailbox availability

The disclosure mailbox `security@shithub.example` auto-acks within
minutes. If you reported a vulnerability and didn't get an
auto-ack within 30 min, the inbound flow is broken — please email
the operator's GitHub-listed contact as a fallback.

## Recent incidents

(none yet)

## Subscribing

There's no email/RSS feed for status updates yet. Watch the
operator's announcements account (linked from the
[homepage](https://shithub.example/)) or refresh this page during
a known incident window.
