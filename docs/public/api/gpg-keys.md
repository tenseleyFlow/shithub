# GPG keys

OpenPGP public-key CRUD for the authenticating user. JSON shape
is GitHub-exact — every field on
[gh's `/user/gpg_keys`](https://docs.github.com/en/rest/users/gpg-keys)
is present with the same name and nullability so existing clients
work without per-field shims.

All endpoints require `Authorization: Bearer <pat>` and the
[common API conventions](overview.md) apply.

## Key shape

```json
{
  "id": 17,
  "name": "laptop",
  "primary_key_id": null,
  "key_id": "ABCDEF0123456789",
  "public_key": "-----BEGIN PGP PUBLIC KEY BLOCK-----\n…",
  "raw_key": "-----BEGIN PGP PUBLIC KEY BLOCK-----\n…",
  "emails": [
    { "email": "alice@example.com", "verified": true }
  ],
  "subkeys": [
    {
      "id": 0,
      "primary_key_id": 17,
      "key_id": "FEDC...",
      "public_key": "",
      "emails": [],
      "subkeys": [],
      "can_sign": true,
      "can_encrypt_comms": false,
      "can_encrypt_storage": false,
      "can_certify": false,
      "created_at": "2026-05-12T04:00:00Z",
      "expires_at": null,
      "raw_key": null,
      "revoked": false
    }
  ],
  "can_sign": false,
  "can_encrypt_comms": false,
  "can_encrypt_storage": false,
  "can_certify": true,
  "created_at": "2026-05-12T04:00:00Z",
  "expires_at": null,
  "revoked": false,
  "raw_key": "-----BEGIN PGP PUBLIC KEY BLOCK-----\n…"
}
```

Field notes:

- `key_id` / `subkeys[].key_id` — uppercase hex. The primary
  surfaces its short 16-hex key id; full 40-hex fingerprints are
  used internally for verification lookups but not exposed in
  the response (gh-parity).
- `public_key` and `raw_key` — both carry the same armored block.
  gh historically distinguishes them; shithub follows suit for
  client compatibility.
- `can_encrypt_comms` and `can_encrypt_storage` — RFC 4880 §5.2.3.21
  splits encryption capability into two bits; both surface
  honestly. Encryption-only keys (no signing subkey) **are
  accepted**; they just can't verify commits.
- `emails[].verified` — `true` when the UID email matches a
  verified email on the authenticating user's account.
- `expires_at` — null for keys that don't expire.
- `revoked` — `true` after a `DELETE` (soft-delete in the DB).

## List GPG keys

```
GET /api/v1/user/gpg_keys
```

Required scope: `user:read`. Paginated via `?page=` and
`?per_page=` (≤ 100, default 30). Response carries the standard
`Link:` header.

## Get a single GPG key

```
GET /api/v1/user/gpg_keys/{id}
```

Required scope: `user:read`. 404 when the id does not belong to
the authenticating user (existence-leak safe — the same status
is returned regardless of whether the row exists for another
user).

## Add a GPG key

```
POST /api/v1/user/gpg_keys
```

Required scope: `user:write`.

### Request body

```json
{
  "name": "laptop",
  "armored_public_key": "-----BEGIN PGP PUBLIC KEY BLOCK-----\n…"
}
```

| Field                | Type   | Notes                                                                  |
|----------------------|--------|------------------------------------------------------------------------|
| `name`               | string | 1–80 characters; user-visible label.                                   |
| `armored_public_key` | string | Full ASCII-armored block, including the BEGIN/END envelope.            |

Returns `201` on success with the same shape as the list
endpoint. Successful inserts dispatch a background backfill
that retroactively stamps verification rows for any existing
commits whose signing subkey matches.

### Errors

| Status | When                                                                                |
|-------:|-------------------------------------------------------------------------------------|
|    401 | PAT missing/invalid.                                                                |
|    403 | PAT lacks `user:write` scope.                                                       |
|    422 | Block unparseable, key already registered, RSA<2048, no UID, expired primary, etc. |

Rejection classes the parser raises explicitly: private-key
blocks, signature blocks, RSA<2048, DSA-only, expired primary,
no-UID entities. Encryption-only keys with valid encryption
subkeys are accepted with `can_sign: false`.

## Delete a GPG key

```
DELETE /api/v1/user/gpg_keys/{id}
```

Required scope: `user:write`. Returns `204 No Content` on
success. Soft-delete: the row stays in the DB with
`revoked_at = now()`. Verification cache rows that resolved
against the deleted key are invalidated; affected commits
revert to no badge until another matching key is uploaded.

404 when the id does not belong to the authenticating user.
