# Regenerate AKC cache

The `AuthorizedKeysCommand` (`shithubd ssh-authkeys %f`) resolves
an offered SSH key fingerprint to a user via the
`user_ssh_keys` table. It does not maintain a write-through
cache today (every call hits the DB), so there's nothing to
"regenerate" in the sense of `cache.invalidate(...)`.

What this runbook actually covers: the **postgres-side**
caching that the AKC subprocess depends on, and the operational
state you might need to reset around it.

## When this matters

You may need to do something here if:

- A user's just-added SSH key is being rejected on push despite
  showing in their Settings.
- A removed SSH key is still being accepted (this would be a
  bug; don't shrug it off).
- The AKC subprocess is timing out under load and you need to
  understand what's slow.

## Diagnose first

```sh
# What's the AKC subcommand seeing?
sudo -u shithub-ssh /usr/local/bin/shithubd ssh-authkeys SHA256:abc...
# This prints what sshd would have read; empty output = no match.
```

Compare against the DB:

```sh
psql -d shithub -c "
  SELECT k.fingerprint, u.username
    FROM user_ssh_keys k
    JOIN users u ON u.id = k.user_id
   WHERE k.fingerprint = 'SHA256:abc...';"
```

Three possibilities:

| psql says | AKC says | Diagnosis                                      |
|-----------|----------|------------------------------------------------|
| match     | match    | sshd is broken, not us — check sshd logs.       |
| match     | empty    | The AKC subcommand isn't reading what we think — check `EnvironmentFile`, the binary path, and that `shithub-ssh` user can read `/etc/shithub/ssh.env`. |
| empty     | empty    | Key isn't in the DB — user needs to re-add.    |
| empty     | match    | **Stale cache or replication lag.** See below. |

## "Empty in psql, match in AKC" — actually impossible today

We don't run a read replica for the AKC. If you ever see this,
the AKC subcommand is reading from a different DB than psql is
— check `db.url` in `ssh.env` vs the operator's psql connection.

## The remove-key-but-still-accepted case

A removed SSH key being accepted is **not** caused by a stale
cache (we don't have one); it's caused by either:

- The key wasn't actually removed (check the DB, not the UI).
- sshd is using its own `authorized_keys` file in addition to
  AKC. Check `/etc/ssh/sshd_config` and per-user
  `~/.ssh/authorized_keys`. The shipped sshd config disables
  per-user files for the `git` user; if someone's customized,
  that's the leak.
- The session was already authenticated and is being held open.
  Kill it: `sudo systemctl status sshd`, find the per-conn
  process, `kill <pid>`.

## Future: add a cache here

If we ever add an in-process cache to AKC (we'd want to, to
shave the per-push DB call), invalidation becomes load-bearing:

- Cache key: SHA-256 fingerprint.
- Invalidate on: key add (insert) and key remove (delete).
- TTL: 60 seconds is the longest acceptable window between
  removing a key and the AKC stopping to honor it.

When that lands, this runbook gets a real "regenerate" section.
