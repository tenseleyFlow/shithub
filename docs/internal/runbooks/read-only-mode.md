# Read-only mode

shithub does not have a one-flag "read-only" toggle. When you
need one — disaster mid-recovery, surprise database failover,
controlled maintenance — these are the levers:

## The fastest "stop writes" lever

```sh
# At the edge: serve every write method as 503 with a banner.
ssh edge
sudo caddy reload --config /etc/caddy/Caddyfile.read-only
```

The repo ships a `Caddyfile.read-only` snippet that responds 503
to `POST`/`PUT`/`PATCH`/`DELETE`/`OPTIONS` while letting `GET`
and `HEAD` through. `git-receive-pack` is also blocked; clones
still work.

Reverse with `caddy reload --config /etc/caddy/Caddyfile`.

User experience:

- Reads work normally.
- Writes return a 503 with an HTML page explaining "shithub is
  in read-only maintenance — try again later."
- Pushes fail with a git-side error.

This is the easiest reversal; nothing in the app changes.

## Stop the worker

If you're worried about *background* writes (job processing
mutating state), stop the worker too — see
[drain-workers.md](./drain-workers.md).

## Set the DB to read-only

Heavy hammer. `ALTER SYSTEM SET default_transaction_read_only =
on; SELECT pg_reload_conf();` makes every new connection
read-only. The web service will blow up on its first write
attempt — usually `ERROR: cannot execute INSERT in a read-only
transaction`.

This is only useful if you suspect a bug in the read-only mode
above is letting writes leak through. Otherwise skip it.

To revert: `ALTER SYSTEM SET default_transaction_read_only = off;
SELECT pg_reload_conf();`.

## During a real DB failover

If you've failed over to a standby that's read-write and need
the app to talk to it:

1. Update the `db.url` inventory variable to point at the new
   primary.
2. `ANSIBLE_TAGS=app make deploy ANSIBLE_INVENTORY=production` —
   web/worker pick up the new connection string.

There's no replication tooling shipped today; if you have a
standby, you set it up out of band.

## What read-only mode does NOT do

- Stop the auth-audit log from writing — login attempts still
  hit the DB.
- Block the `shithubd hook` invocations from writing
  `push_events` (because the AKC + git transport still let the
  TCP connection through). To stop that, also stop the web
  service or block port 22.
- Block sign-ups — those are POSTs and are blocked at the edge,
  but the in-app banner under read-only mode is the only signal
  to a logged-in user.

## Communicating it

In-app banner on every page during read-only mode is the right
default. Today this is via a feature flag (`web.banner.text`
config key); set it before flipping the Caddyfile so users see
the banner on the last successful render.
