# Actions runbook

## Live log tail

Step log pages open an SSE stream at:

```text
/{owner}/{repo}/actions/runs/{run}/jobs/{job}/steps/{step}/log/stream
```

The stream sends `event: chunk` records with the chunk sequence as the SSE
`id`. Browsers reconnect with `Last-Event-ID`; the handler also accepts
`?after=<seq>` for the first connection from a rendered log page. A terminal
step sends `event: done` and closes the stream.

In `shithubd`, this route is mounted outside the normal app compression and
30-second timeout middleware. If a future route move puts live logs back under
either middleware, EventSource clients will churn and logs can buffer despite
the Caddy flush setting.

Log chunks are never sent through Postgres `NOTIFY`. Runner log writes append
to `workflow_step_log_chunks`, then `NOTIFY step_log_<step_id>` with only the
sequence number. Step completion notifies `done`.

## Rate limit

Live tails use `internal/ratelimit` scope `actions:logtail` with five
concurrent streams per viewer. Authenticated viewers key by user id; anonymous
public-repo viewers key by client IP. The limiter uses a short lease TTL so a
dropped connection cannot hold a slot permanently.

## Caddy

The production Caddy template has a dedicated Actions log-stream route with:

```caddy
flush_interval -1
```

The same route is excluded from gzip compression. If logs arrive only after
several kilobytes accumulate, verify the deployed `/etc/caddy/Caddyfile`
contains that route and reload Caddy:

```sh
sudo caddy reload --config /etc/caddy/Caddyfile
```
