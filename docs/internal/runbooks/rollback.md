# Rollback

When to rollback vs. roll-forward:

- **Rollback** when the new release is broken in a way that can't
  be hot-fixed in <30 min: a panic loop, an auth regression, data
  corruption.
- **Roll-forward** when you can ship a hotfix faster than the
  rollback ceremony. A small bug behind a known feature flag is a
  roll-forward case.

If unsure, roll back. Bad releases compound.

## Code rollback (no migration involved)

```sh
git checkout v<previous>
make deploy ANSIBLE_INVENTORY=production
```

The systemd unit `Restart=on-failure` + the binary swap means the
process flips on the next ExecStart. Connections in flight finish;
new connections hit the rolled-back binary.

## Code rollback when the new release added migrations

This is the dangerous case. There are three options, in order of
preference:

### 1. Schema-compatible rollback (best)

If the new migration only *added* columns/tables that the old code
ignores, the old code runs against the new schema fine. Just roll
the code back; leave the schema alone.

Most of our migrations are deliberately additive for this reason.

### 2. Roll forward to a hotfix

If the migration changed semantics that the old code can't tolerate,
ship a hotfix on top of the new release rather than reversing the
migration.

### 3. Migration `down` + code rollback (last resort)

Only if (1) and (2) won't work and the data loss from `down` is
acceptable.

```sh
ssh web-01
sudo -u shithub /usr/local/bin/shithubd migrate down  # ONE step
# verify
git checkout v<previous>
make deploy ANSIBLE_INVENTORY=production
```

`migrate down` rolls back exactly one step. **Never** chain `down`s
without checking each migration's down logic; some of them drop
columns and *will* lose data.

## After any rollback

- Note the rollback in the incident channel with the from-tag and
  to-tag.
- File a follow-up issue with the failure mode.
- Disable any feature flags the bad release turned on.
- Confirm the rolled-back release passed CI (if not, you're now
  running un-tested code — that's a separate incident).
