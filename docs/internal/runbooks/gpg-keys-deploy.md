# Deploy: GPG keys + commit signature verification (S51)

S51 ships the GPG-keys storage + the verification orchestrator
+ the Verified-badge rendering + a bulk-backfill admin command,
all in one feature-branch merge to trunk. The migrations are
additive and the rendering paths fall back to "unsigned"
(no badge) on cache miss, so the rollout is safe to do
in-band — no maintenance window required.

## Pre-deploy

1. Note the currently-deployed `ldd /opt/shithub/bin/shithubd`
   output. shithub uses pure-Go OpenPGP via
   `github.com/ProtonMail/go-crypto`; no new dynamic deps are
   expected. Diffing post-deploy confirms.

   ```sh
   ssh root@shithub.sh "ldd /opt/shithub/bin/shithubd" > /tmp/ldd-pre.txt
   ```

2. Capture migration status as a baseline:

   ```sh
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd migrate status" \
     > /tmp/migrate-pre.txt
   ```

## Deploy

3. Apply migrations. Three new versions land:
   - `0068_user_gpg_keys.sql` — primary keys table
   - `0069_user_gpg_subkeys.sql` — subkey fingerprint reverse-lookup
   - `0070_commit_verification_cache.sql` — verification result cache

   ```sh
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd migrate up"
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd migrate status"
   ```

   `migrate status` should report all three at version applied.

4. Quick sanity-check the three new tables exist and are empty:

   ```sh
   ssh root@shithub.sh "sudo -u shithub psql -d shithub -c '
     SELECT count(*) FROM user_gpg_keys;
     SELECT count(*) FROM user_gpg_subkeys;
     SELECT count(*) FROM commit_verification_cache;'"
   ```

   Expect `0 / 0 / 0`.

5. Rolling-restart shithubd. The deploy unit drops one replica
   at a time; the health endpoint reports 200 once a replica
   has finished its startup migration check.

   ```sh
   ssh root@shithub.sh "sudo systemctl restart shithubd@*"
   ```

6. Confirm the `gpg-keys` capability landed:

   ```sh
   curl -fsS https://shithub.sh/api/v1/meta | jq '.capabilities | index("gpg-keys")'
   ```

   Non-null output confirms.

## Bulk backfill

7. **Once-off after deploy.** Walk every repo's default branch
   and stamp the verification cache for commits whose signing
   subkey matches an already-uploaded GPG key. This is what
   lights up existing signed commits for users whose keys we
   already had (mostly admins / early testers; the long tail
   gets covered on next key upload).

   ```sh
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd gpg-backfill-all"
   ```

   The command is idempotent and resumable — re-running picks
   up where it left off (cache rows are upserted; existing rows
   short-circuit the walk for that commit). Progress is logged
   per repo. On a fresh deploy with no pre-existing GPG keys
   the command is a no-op and exits in a few seconds.

   To limit to a single repo (e.g. for spot-checking after the
   bulk run):

   ```sh
   ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd \
     gpg-backfill-all --repo=<owner>/<name>"
   ```

## Smoke-test

8. Log in as a test account and exercise the user-visible flow:

   - Navigate to `/settings/keys`. Confirm the page renders with
     the combined SSH + GPG sections.
   - Upload a test GPG key whose UID email matches a verified
     email on the test account.
   - Navigate to a repo that has signed commits authored by
     that account on its default branch. The commit list should
     show green **Verified** pills within a few seconds of the
     upload (the backfill worker picks up the eager dispatch).
   - Open `/{owner}/{repo}/commits/{sha}` for one of those
     commits. The header should show the larger Verified badge
     with a click-popover.

9. Hit the REST surface with a `repo:read` PAT:

   ```sh
   curl -fsS -H "Authorization: Bearer $PAT" \
     https://shithub.sh/api/v1/repos/<owner>/<name>/commits | \
     jq '.[0].verification'
   ```

   Expect `{verified, reason, signature, payload, verified_at}`
   on every entry.

## Rollback

The migrations are additive; rollback is `migrate down 3` to
reverse them. Re-running with a server build that includes
0066–0068 will re-apply on the next deploy with no data loss
(the tables get rebuilt empty; the next eager dispatch repopulates).

If a bad server build forces a rollback to a pre-S51 binary:

```sh
ssh root@shithub.sh "sudo -u shithub /opt/shithub/bin/shithubd migrate down 3"
```

This drops the three new tables. Any verification cache rows
are lost; the bulk backfill on the subsequent re-deploy
regenerates them.

## See also

- User docs: [GPG keys & commit signing](/docs/public/user/gpg-keys.md)
- API reference: [GPG keys](/docs/public/api/gpg-keys.md),
  [Signature verification on commits](/docs/public/api/commits.md#signature-verification)
