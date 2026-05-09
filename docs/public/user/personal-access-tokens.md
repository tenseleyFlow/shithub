# Personal access tokens

Personal access tokens (PATs) are scoped, expirable credentials
you create from your account settings. They're how you
authenticate to the API and to git over HTTPS.

PATs are **not** passwords:

- They have an expiration.
- They have scopes — a token cannot do what its scopes don't grant.
- They are listed in your settings with a "last used" timestamp.
- They can be revoked individually without changing your password.

## Scopes

Pick the smallest set the consumer needs.

| Scope          | What it allows                                                       |
|----------------|----------------------------------------------------------------------|
| `repo:read`    | Read repos you can already see (public + your private + collabs).    |
| `repo`         | Above + push, manage settings on repos you own/admin.                |
| `user:read`    | Read your profile + email.                                           |
| `user`         | Above + edit profile, emails.                                        |
| `notifications`| Read + mark-read your notification inbox.                            |
| `webhooks`     | Manage webhooks on repos you own/admin.                              |
| `admin:org`    | Org management (membership, teams) for orgs you admin.               |
| `gist`         | Reserved for future Gists feature; non-functional today.             |

Scopes only **grant**; they never elevate. A `repo` scope on your
PAT cannot push to a repo you don't have write access to.

## Creating a PAT

Settings → Developer settings → Personal access tokens → "New
token".

- **Note** — what is this token for? "ci-runner staging" beats
  "test1".
- **Expiration** — pick the smallest tolerable; 90 days is a
  reasonable default. "Never" is available but discouraged.
- **Scopes** — check only what you need.

The token is displayed **once**. Copy it now; we cannot show it
to you again. If you lose it, revoke and re-create.

## Token format

Tokens are 40 characters of base32 with a `shp_` prefix:

```
shp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

The prefix is recognized by GitHub-style secret-scanning tools.
If you accidentally publish a token, secret scanners may notify
you (and us); revoke immediately.

## Using a PAT

- **Git over HTTPS:** username = your shithub username, password
  = the PAT. See [HTTPS clone](./https.md).
- **API:** `Authorization: Bearer <token>` or `Authorization:
  token <token>`.

## Revoking

Settings → Developer settings → Personal access tokens shows every
PAT on the account. Click "Revoke" — the token stops working
immediately. Anything using it will get `401`.

If you suspect a token leaked, revoke first and investigate after.
