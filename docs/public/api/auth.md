# Authentication

shithub's API authenticates calls with Personal Access Tokens.
PATs are minted via the web UI or, for CLI / non-browser clients,
through the [device-code grant](#device-code-grant-rfc-8628).

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

Two paths exist:

1. **Web UI** — sign in at `/login`, then mint a token at
   `/settings/tokens`. Returns the raw token exactly once.
2. **Device-code grant** — the standardised CLI / TV / IoT flow,
   documented in the next section. The client_id must be in the
   server's allowlist (default: `shithub-cli`).

## Device-code grant (RFC 8628)

The device-code grant lets a CLI obtain a PAT without prompting
the user to paste a token. It mirrors GitHub's `/login/device/*`
shape verbatim.

### Endpoints

```
POST /login/device/code                request a new authorization
POST /login/oauth/access_token         poll until the user approves
GET  /login/device                     browser verification page
```

`/login/device/code` and `/login/oauth/access_token` are CSRF-exempt
and accept `application/x-www-form-urlencoded` bodies.

### 1. Request a device code

```
POST /login/device/code
Content-Type: application/x-www-form-urlencoded

client_id=shithub-cli&scope=user%3Aread,repo%3Aread
```

```json
{
  "device_code":               "f0a1b2c3...",
  "user_code":                 "ABCD-EFGH",
  "verification_uri":          "https://shithub.example/login/device",
  "verification_uri_complete": "https://shithub.example/login/device?user_code=ABCD-EFGH",
  "expires_in": 900,
  "interval":   5
}
```

- `client_id` must be in the server's allowlist. Default: `shithub-cli`.
- `scope` is space- or comma-separated. Omit to receive
  `user:read`. Unknown scopes return `invalid_scope`.
- `device_code` is returned once and never echoed back; store it
  in client memory only.
- `interval` is the minimum seconds between polls — see
  `slow_down` below.

### 2. Show the user the code

Open the user's browser to `verification_uri_complete` (or
`verification_uri` if you can't open URLs with query strings).
The user enters the `user_code`, signs in if needed, then clicks
**Authorize** or **Deny**.

### 3. Poll the exchange endpoint

```
POST /login/oauth/access_token
Content-Type: application/x-www-form-urlencoded

grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code
&client_id=shithub-cli
&device_code=f0a1b2c3...
```

Successful exchange (after the user has approved):

```json
{
  "access_token": "shithub_pat_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX",
  "token_type":   "bearer",
  "scope":        "user:read,repo:read"
}
```

The access token is disclosed **exactly once**. A second exchange
of the same `device_code` returns `invalid_grant` even after
successful approval — clients must cache the token from the
first 200 response.

### Error codes

All errors are HTTP 400 with a JSON body:

```json
{ "error": "<code>", "error_description": "..." }
```

| `error`                | Meaning                                                                       |
|------------------------|-------------------------------------------------------------------------------|
| `authorization_pending`| User has not approved or denied yet. Keep polling at `interval` seconds.      |
| `slow_down`            | You polled inside the `interval` window. Increase your delay and retry.       |
| `access_denied`        | The user explicitly denied the request. Stop polling.                         |
| `expired_token`        | The grant outlived its `expires_in`. Restart from `/login/device/code`.       |
| `invalid_grant`        | `device_code` unknown or already exchanged.                                   |
| `unauthorized_client`  | `client_id` is not in the server's allowlist.                                 |
| `invalid_scope`        | One or more requested scopes is not a known shithub scope.                    |
| `unsupported_grant_type`| The exchange request used a non-device-code grant_type.                       |
| `invalid_request`      | Required form fields missing or body malformed.                               |

### Lifecycle invariants

- The device_code is single-use for token disclosure. After a
  successful exchange the row stays in the database for forensics
  but further exchanges always return `invalid_grant`.
- The user_code is human-typeable: 8 characters from a 32-symbol
  alphabet that excludes 0/O/1/I, formatted `XXXX-XXXX`. The
  verification page also accepts the unhyphenated form.
- Issued PATs carry the scopes requested at `/login/device/code`
  time. The token is named on the user's `/settings/tokens` page
  with a recognisable label derived from the CLI's `User-Agent`.
