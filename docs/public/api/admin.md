# Admin (site-admin only)

> **Planned.** The admin API is not exposed yet. Site-admin
> actions today are reachable through the `/admin/` web UI and
> the `shithubd admin` CLI subcommands.

The site-admin surface is intentionally narrow: most operator
actions go through the CLI (`shithubd admin …`) where they're
auditable from journal logs. The planned API here exists for
automation that's already authenticated as a site admin (e.g.,
an SSO/SCIM bridge).

## Planned routes

| Method | Path                                     | Scope          | Purpose                          |
|--------|------------------------------------------|----------------|----------------------------------|
| GET    | `/api/v1/admin/users`                    | site-admin     | List users (paginated).          |
| GET    | `/api/v1/admin/users/{id}`               | site-admin     | One user, with admin-only fields.|
| POST   | `/api/v1/admin/users/{id}/suspend`       | site-admin     | Freeze the account.              |
| POST   | `/api/v1/admin/users/{id}/reinstate`     | site-admin     | Un-freeze.                       |
| POST   | `/api/v1/admin/users/{id}/reset-password`| site-admin     | Force a password reset email.    |
| POST   | `/api/v1/admin/users/{id}/site-admin`    | site-admin     | Grant or revoke site-admin bit.  |

## Authorization

A regular PAT — even one held by a user who is a site admin — is
**not enough** by itself; the admin endpoints require both:

- The token's owner has the `is_site_admin` flag set.
- The token has the `admin:site` scope (separate from `admin:org`).

Both checks must pass; either alone returns 403.

## Audit

Every admin API call writes to the same `admin_audit_log` the
web admin UI uses. Each row carries the calling site admin's id,
the target id, the action, and the request IP.
