# Backing up the production .env

`/etc/shithub/web.env` (and `/etc/shithub/worker.env`, when split)
holds every load-bearing secret on the droplet:

- DB password (`SHITHUB_DATABASE_URL`)
- Session signing key, TOTP AEAD key, webhook AEAD key
- Spaces access keys (object store)
- Postmark / SMTP creds
- SSH-git host key seed (when configured)

The DB content is captured by [`backups.md`](backups.md) — those
dumps and WAL segments rebuild the data. `web.env` is what
re-binds a fresh droplet to that data: without the same secrets,
the existing DB rows can't be decrypted (TOTP/webhook payloads),
sessions can't be signed, and S3 buckets can't be reached.

This doc covers backing up `web.env` itself.

## The mental model

Treat `web.env` like a master password. The DB backup chain is
fully automated and tested; the env file is operator-managed
because it has different lifecycle:

- **Mostly stable**: changes only when secrets rotate (see
  `rotate-secrets.md`) or new config keys land.
- **Tiny**: a few KB.
- **Maximum sensitivity**: leaking it is a "rotate everything"
  incident. So it shouldn't ride alongside the DB dumps in
  Spaces — different blast radius.

That puts it in a different storage tier than DB dumps. Pick one
of the three options below.

## Option A (recommended) — password manager

Keep an encrypted copy in your operator password manager
(1Password, Bitwarden, KeePassXC, …) as a "Secure Note" or
file attachment.

**When to update:**
- After every secret rotation per `rotate-secrets.md`
- After any change to `web.env` for new config keys
- Right after a fresh droplet provision (initial baseline)

**How:**
```sh
ssh root@shithub.sh 'cat /etc/shithub/web.env'
# Paste output into a Secure Note titled e.g. "shithub-prod web.env (YYYY-MM-DD)"
# Tag with the rotation date so you can identify the active copy.
```

Pros: zero new infrastructure, auditable access (PM logs),
already in your daily-use security tooling. Cons: manual
step that's easy to skip after a rotation — set a calendar
reminder for the quarterly rotation cadence.

## Option B — encrypted blob alongside DB backups

Include an encrypted copy of `web.env` in the daily backup
chain. Encrypt with `age` or `gpg` so even a compromised Spaces
key can't decrypt it; the recipient key is held only by
operators (in their PM).

Sketch (NOT yet wired up — would be a follow-up if we go this
route):

```sh
# In shithub-backup-daily, after the pg_dump succeeds:
age -r "$(cat /etc/shithub/env-backup.recipients)" \
    -o "$LOCAL_DIR/web.env.${STAMP}.age" \
    /etc/shithub/web.env
rclone copyto "$LOCAL_DIR/web.env.${STAMP}.age" \
       "$BUCKET/env/$(date -u +%Y/%m/%d)/web.env.${STAMP}.age"
```

Pros: automatic, captures rotations as they happen. Cons: adds
a dep (`age`), a new key-mgmt surface (the recipient key), and
a failure mode (if the recipient key is lost, the backups
become useless).

## Option C — DO snapshot

DO droplet snapshots include `/etc/shithub/web.env` by virtue of
including the whole filesystem. This is "free" coverage but the
caveats are real:

- **Point-in-time only**: a snapshot taken before a secret
  rotation has the OLD secret. Restoring a stale snapshot
  desynchronizes you from any DB rows encrypted with the new
  key (TOTP, webhook payloads).
- **Snapshots are scheduled to be deleted** under DO's free
  policy (4 retained, oldest pruned).
- **Snapshot restore replaces the droplet**, including the
  block volume's previous state (depending on snapshot type).

Suitable as a *belt-and-suspenders* on top of A or B. Not
sufficient as the only backup of the env file.

## Restore procedure

If the live `web.env` is lost (droplet replaced, file deleted,
permissions wedged):

1. Stop the running services that need it:
   ```sh
   systemctl stop shithubd-web shithubd-cron
   ```
2. Recreate `/etc/shithub/web.env` with the right ownership and
   mode (see `deploy/ansible/roles/shithubd/tasks/main.yml` for
   canonical perms — currently `root:shithub 0640`):
   ```sh
   install -o root -g shithub -m 0640 /dev/stdin /etc/shithub/web.env <<'EOF'
   <paste from your Secure Note>
   EOF
   ```
3. Restart:
   ```sh
   systemctl start shithubd-web shithubd-cron
   curl -fsS http://127.0.0.1:8080/healthz   # 200
   ```
4. If the secrets in the restored copy are stale (you rotated
   after the backup), follow `rotate-secrets.md` to re-apply
   the current values.

## What goes wrong if you skip this

Concrete scenarios where lacking an env backup turns a recoverable
incident into a re-key-the-world incident:

| Scenario | With env backup | Without |
|---|---|---|
| Droplet kernel panic, fsck loses files | Restore web.env, restart, done | Rotate every secret in `rotate-secrets.md`, plus likely re-encrypt every TOTP secret + webhook payload |
| Operator deletes `/etc/shithub` by mistake | Same as above | Same as above |
| DO destroys droplet (account billing issue) | New droplet + restored env + restored DB → up | Same DB-restore work, plus full secret rotation |
| Provider breach forces DB password rotation | Update one line in PM, redeploy | Same — env backup neutral here, but you should still update the PM copy after the rotation |

## How this relates to the rest of the backup story

| What | Where it's backed up | Cadence |
|---|---|---|
| DB rows | `spaces-prod:shithub-backups/daily/...` (pg_dump) + `spaces-prod:shithub-wal/` (WAL) | Continuous + daily |
| Bare repos | `/data/repos` on the block volume + cross-region Spaces sync | Continuous |
| Object store contents | DO Spaces lifecycle handles versioning | Provider-managed |
| Operator secrets (`web.env`) | Operator password manager (this doc) | Per-rotation |
| Filesystem layout | DO droplet snapshots | Weekly or pre-major-change |
