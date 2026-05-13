# Commits

Read-only commits surface. Mirrors GitHub's
`/repos/{owner}/{repo}/commits` family — backed by `git log`
on the bare repository so the response is always in lock-step
with the on-disk history.

All endpoints require `Authorization: Bearer <pat>` with
`repo:read` and gate on `ActionRepoRead`. The
[common API conventions](overview.md) apply.

## List commits

```
GET /api/v1/repos/{owner}/{repo}/commits[?sha=&path=&author=&since=&until=&page=&per_page=]
```

Query parameters:

- `sha` — branch / tag / commit SHA to start from. Default:
  the repo's `default_branch`.
- `path` — show only commits touching this path (passes
  through to `git log -- <path>`).
- `author` — `git log --author=<...>` filter (substring match
  on author name or email).
- `since` / `until` — RFC3339 timestamps bracketing the window.
- `page` / `per_page` — standard pagination (per_page ≤ 100,
  default 30). Since `git log` doesn't expose a cheap total
  count, the `Link:` header only emits `next` / `prev` rels,
  never `last`.

Response shape (one entry):

```json
{
  "sha":       "5f3a…",
  "short_sha": "5f3aabc",
  "subject":   "fix race in fanout worker",
  "body":      "longer commit body wraps here",
  "author":    {
    "name":  "Alice",
    "email": "alice@example.com",
    "date":  "2026-05-12T00:00:00Z"
  },
  "verification": {
    "verified":    false,
    "reason":      "unsigned",
    "signature":   null,
    "payload":     null,
    "verified_at": null
  }
}
```

The `verification` object mirrors GitHub's documented shape and
is always present. See [Signature verification](#signature-verification)
below for the field semantics and the `reason` enum.

Empty / uninitialised repos return `[]` rather than `404`.

## Get a commit

```
GET /api/v1/repos/{owner}/{repo}/commits/{sha}
```

`{sha}` accepts a full 40-char SHA or any unambiguous prefix
that `git rev-parse` resolves. Unknown SHAs `404`.

```json
{
  "sha":       "5f3a…",
  "short_sha": "5f3aabc",
  "subject":   "fix race in fanout worker",
  "body":      "…",
  "author":    { "name": "Alice", "email": "alice@example.com", "date": "2026-05-12T00:00:00Z" },
  "committer": { "name": "Alice", "email": "alice@example.com", "date": "2026-05-12T00:00:00Z" },
  "parents":   ["9c1b…"],
  "tree_sha":  "abcd…",
  "files": [
    {
      "path":      "internal/webhook/fanout.go",
      "status":    "M",
      "additions": 7,
      "deletions": 3,
      "binary":    false
    }
  ],
  "stats": { "additions": 7, "deletions": 3, "total": 10 }
}
```

`files[].status` is git's letter code (`A` added, `M` modified,
`D` deleted, `R` renamed, `C` copied, `T` type-changed). Renames
and copies surface the original path on `old_path`.

## Signature verification

Every commit response carries a `verification` object. shithub
runs the signature check server-side against the bytes git
stored in the commit object; the result is cached per
`(repo, commit_oid)` and surfaced here.

```json
{
  "verified":    true,
  "reason":      "valid",
  "signature":   "-----BEGIN PGP SIGNATURE-----\n…",
  "payload":     "tree abc…\nparent def…\n…",
  "verified_at": "2026-05-12T04:00:00Z"
}
```

| Field         | Type             | Notes                                                                          |
|---------------|------------------|--------------------------------------------------------------------------------|
| `verified`    | bool             | `true` only when `reason == "valid"`. Always `false` otherwise.                |
| `reason`      | string           | One of the enum values below.                                                  |
| `signature`   | string \| null   | The armored signature block as stored on the commit object.                    |
| `payload`     | string \| null   | The bytes the signature was computed over (commit object minus `gpgsig`).      |
| `verified_at` | string \| null   | RFC3339 timestamp of the cache row; `null` for unsigned / not-yet-stamped.     |

### `reason` enum

The values mirror gh's documented enum exactly:

| Value                 | Meaning                                                                                                  |
|-----------------------|----------------------------------------------------------------------------------------------------------|
| `valid`               | Signature parsed, cryptographically valid, signing email matches a verified email on the key's account.  |
| `unsigned`            | Commit object carried no signature header. Default for cache misses; clients render no badge.            |
| `unknown_key`         | Signature parsed but no uploaded GPG key matches the signing subkey's fingerprint.                       |
| `unverified_email`    | Signature is valid for an uploaded key, but the signing email isn't verified on that key's account.      |
| `bad_email`           | Signature is valid for an uploaded key, but the signing email isn't on the key at all.                   |
| `expired_key`         | Signature parsed but the key was expired at signing time.                                                |
| `not_signing_key`     | The key referenced isn't a signing key (capability bits missing).                                        |
| `malformed_signature` | The `gpgsig` header didn't parse as an OpenPGP signature block.                                          |
| `invalid`             | Signature parsed but the cryptographic check failed.                                                     |

### Cache freshness

Verification rows are populated by an asynchronous backfill that
runs whenever a user uploads a GPG key (and once-off at deploy
time via `shithubd gpg-backfill-all`). Between key upload and
backfill completion, affected commits report `unsigned` (the
conservative default); the badge appears once the row is
stamped. Revoking a key invalidates affected cache rows;
clients see `unsigned` until another matching key is uploaded.
