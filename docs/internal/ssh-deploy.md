# SSH deploy notes

S07 ships the SSH-key data layer + the `AuthorizedKeysCommand` (AKC) integration. To turn it into a working git-over-SSH endpoint, sshd needs a small amount of operator setup. This doc captures what that looks like and the security knobs that matter. The actual git protocol handler lands in S13.

## Architecture

```
ssh client
    │  TCP/22
    ▼
sshd (system) ──► AuthorizedKeysCommand: shithubd ssh-authkeys <fingerprint>
                  ─ stdout: a single authorized_keys line, OR empty
                  ─ exit 0 in both cases (failing closed on any error)
                  └──► forced command per the line:
                       shithubd ssh-shell <user_id>
                       (S13 dispatcher; S07 placeholder logs and exits non-zero)
```

Why AKC instead of a static `~/.ssh/authorized_keys`:

- The static-file approach requires regenerating a file on every key change and has weak consistency guarantees with replicas.
- AKC is sshd's purpose-built mechanism for dynamic lookups; failure semantics are well-understood.
- The forced command + restrictive options strip every interactive affordance, so a hijacked key can only invoke `shithubd ssh-shell` — never get a real shell.

## Linux user setup

A dedicated low-privilege system user runs the AKC binary. It owns no files, has no shell, and never logs in interactively.

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin shithub-ssh
```

The shithub binary lives somewhere both root and `shithub-ssh` can read:

```sh
sudo install -m 0755 ./bin/shithubd /usr/local/bin/shithubd
```

Configuration (`SHITHUB_DATABASE_URL`, etc.) lives in a 0600-mode file readable only by `shithub-ssh`:

```sh
sudo install -d -m 0750 -o shithub-ssh -g shithub-ssh /etc/shithub
sudoedit /etc/shithub/ssh.env   # 0600, owned by shithub-ssh
```

Recommended `/etc/shithub/ssh.env`:

```sh
SHITHUB_DATABASE_URL=postgres://shithub_ssh:****@127.0.0.1:5432/shithub?sslmode=disable
```

## sshd configuration

```
# /etc/ssh/sshd_config.d/shithub.conf

# Run AKC for ALL connections that match the standard auth path.
AuthorizedKeysCommand /usr/local/bin/shithubd ssh-authkeys %f
AuthorizedKeysCommandUser shithub-ssh

# Disable password auth and host-based auth — only key-based.
PasswordAuthentication no
PubkeyAuthentication yes

# AKC only sees the connecting client's pubkey FINGERPRINT (%f), not
# the full key. shithubd looks it up by fingerprint and emits the matching
# stored key + forced-command line, which sshd then uses to authenticate.

# Mitigate connection floods. Real numbers depend on traffic profile —
# these are reasonable defaults.
MaxStartups 30:30:100
LoginGraceTime 20s

# Standard hardening. Already on most modern distros; spelled out here for
# review:
PermitRootLogin no
PermitEmptyPasswords no
ChallengeResponseAuthentication no
UsePAM no                     # we own auth end-to-end via AKC
ClientAliveInterval 60
ClientAliveCountMax 3
```

Wrapping the AKC invocation through `EnvironmentFile` to load
`SHITHUB_DATABASE_URL` is the cleanest way to keep secrets out of `argv`:
the simplest path is a 2-line wrapper script in `/usr/local/bin/`:

```sh
#!/bin/sh
# /usr/local/bin/shithub-ssh-authkeys
set -e
. /etc/shithub/ssh.env
exec /usr/local/bin/shithubd ssh-authkeys "$1"
```

Then `AuthorizedKeysCommand /usr/local/bin/shithub-ssh-authkeys %f`. The
wrapper is owned by root and 0755 so sshd can verify the path's ownership
chain (sshd refuses to invoke an AKC whose path or any parent is
group/world writable).

Validate before reloading:

```sh
sudo sshd -t                  # config syntax check
sudo systemctl reload sshd    # apply
```

## Postgres role separation

The AKC binary runs under a low-privilege role with the minimum surface
needed to look up keys and update last-used columns. Everything else
(insert / delete / etc.) goes through the web binary's normal role.

```sql
CREATE ROLE shithub_ssh LOGIN PASSWORD '****';
GRANT CONNECT ON DATABASE shithub TO shithub_ssh;
GRANT USAGE ON SCHEMA public TO shithub_ssh;
GRANT SELECT ON user_ssh_keys TO shithub_ssh;
GRANT UPDATE (last_used_at, last_used_ip) ON user_ssh_keys TO shithub_ssh;
```

Read-only would be cleaner but precludes the last-used update. The
column-level UPDATE grant is the smallest privilege that still serves the
operational need.

## AKC behavior contract

The AKC subcommand has three outcomes:

| Input | Output | Exit |
|---|---|---|
| Known fingerprint | `command="..." options... <algo> <b64>` (single line) | 0 |
| Unknown fingerprint | empty stdout | 0 |
| Any error (DB down, panic, malformed input) | empty stdout | 0 |

**sshd reads stdout content as the auth answer; non-zero exit is a
configuration error, not a deny.** Failing closed (return empty on
unrecoverable conditions) is therefore the correct posture: better to
deny a legitimate connection than to authorize the wrong user.

The forced-command line emitted on a hit:

```
command="/usr/local/bin/shithubd ssh-shell <user_id>",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty <algo> <b64>
```

The option set strips every interactive affordance — defense in depth on
top of `shithubd ssh-shell`'s own command parsing.

## Latency

AKC runs on every SSH connection. Targets:

- p99 < 100 ms with a warm DB on the production droplet.
- Per-process pool sized small (`MaxConns: 4`) — sshd spawns a fresh
  process per connection, so the pool's lifetime is the connection's.
- Connect timeout capped at 750 ms so a flaky DB doesn't stall sshd.

`shithub_http_*` metrics are emitted by the web binary, not by AKC. Add a
synthetic check that calls `shithubd ssh-authkeys <test-fp>` periodically
and alerts if it exceeds the SLO.

## Last-used tracking

The AKC subcommand updates `user_ssh_keys.last_used_at` and
`last_used_ip` after a successful lookup. It runs in a fire-and-forget
goroutine with a 500 ms timeout; any error is silently dropped. This
keeps the hot path fast at the cost of occasional missed updates under
DB pressure — acceptable for a UI-only display field.

If a future sprint needs strict last-used accuracy (anomaly detection,
forensic timeline), promote the path to a small append-only log + worker
roll-up. Don't make AKC's success contingent on the update.

## Operational notes

- **Deleting a key while a session is active:** sshd doesn't re-auth
  mid-session. Existing sessions persist until disconnect. Document this
  in user-facing security help; it's expected SSH behavior.
- **Fingerprint canonicalization:** the codebase uses
  `SHA256:<base64-no-padding>`. Both `ssh.FingerprintSHA256` and the
  `ssh-keygen -E sha256 -lf <pubfile>` output produce this exact format.
  When matching by hand, copy from the fingerprint shown in the user's
  SSH-keys settings page, NOT from `ssh-keygen -lf` (which uses MD5
  unless `-E sha256` is passed).
- **Fail2ban / connection-rate limiting:** lives at the OS layer. Document
  desired thresholds for ops in the S37 deploy bundle. AKC alone does
  not throttle — that's sshd's `MaxStartups` and the firewall's job.

## Smoke test

```sh
# 1. Bring up dev Postgres + apply migrations.
make dev-db && make build && SHITHUB_DATABASE_URL=... ./bin/shithubd migrate up

# 2. Add a key (via the UI or a direct INSERT for testing).

# 3. Invoke the AKC subcommand with a known fingerprint:
SHITHUB_DATABASE_URL=... ./bin/shithubd ssh-authkeys "SHA256:<...>"
# Expect: a single authorized_keys line on stdout, exit 0.

# 4. Invoke with an unknown fingerprint:
SHITHUB_DATABASE_URL=... ./bin/shithubd ssh-authkeys "SHA256:not-a-real-fingerprint-xxxx"
# Expect: empty stdout, exit 0.

# 5. Connect via SSH (placeholder shell):
ssh -p 22 git@<host>
# Expect: stderr line "shithubd ssh-shell: user_id=N original_command=..."
# Exit non-zero with "git over SSH not enabled yet" — replaced fully in S13.
```

## Pitfalls / risks

- **Wrong-user authorization** is the catastrophic bug. Audit the
  unknown-fingerprint and DB-error paths in any future change to
  `cmd/shithubd/ssh.go`.
- **Stray newlines / unescaped options** in the emitted line break sshd
  parsing in subtle ways. The codebase emits a strict template; don't
  introduce dynamic options without escaping.
- **Group/world-writable paths in the AKC chain** make sshd refuse to
  invoke AKC. The wrapper script and binary must be 0755, parents 0755.
- **Postgres role drift:** if `shithub_ssh` ever gains broader privileges,
  a compromise of the AKC binary becomes a much bigger event. Audit
  grants on every deploy.

## Related docs

- `docs/internal/auth.md` — email/password auth (S05).
- `docs/internal/2fa.md` — TOTP + recovery codes (S06).
- `docs/internal/observability.md` — slog redaction.
