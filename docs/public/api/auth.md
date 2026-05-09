# Authentication

shithub's API is PAT-only. There is no OAuth / device-flow / JWT
issuance endpoint today.

## Header

```
Authorization: Bearer shp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

`Authorization: token shp_…` is accepted as a synonym for tools
that hard-code GitHub's older syntax.

## Token format

PATs are 40 characters of base32 with the `shp_` prefix. They
match the regex:

```
shp_[A-Za-z0-9]{40}
```

Secret-scanning tools (GitHub's, GitGuardian's, etc.) recognize
this prefix.

## Failure modes

| Status | Body                              | Cause                                                |
|-------:|-----------------------------------|------------------------------------------------------|
|    401 | `{"error":"unauthenticated"}`     | Missing or malformed header.                          |
|    401 | `{"error":"invalid token"}`       | Token doesn't exist, was revoked, or has expired.    |
|    401 | `{"error":"account suspended"}`   | The owning account has been suspended by an admin.   |
|    403 | `{"error":"insufficient scope"}`  | Token is valid but lacks the scope this route needs. |

## Sessions

The web UI uses session cookies, not PATs. Session cookies are
**not accepted** on `/api/v1/` — the API is PAT-only. This is a
deliberate choice: it keeps CSRF concerns off the API surface
and means every API caller is identified by an auditable token.

## Creating a token programmatically

There is no API for creating PATs; tokens are only created from
the web UI. This is intentional — the create-PAT surface is the
account's most security-sensitive non-password operation.

## Future

OAuth-style application authorizations (`client_id` + `client_
secret`, `code` exchange, refresh tokens) are planned post-MVP.
For now, instruct human users to mint a PAT from their settings
and supply it to your app via the operator's secret-management
flow.
