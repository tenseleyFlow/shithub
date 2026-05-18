# Actions secrets + variables

REST surface for the `${{ secrets.NAME }}` and `${{ vars.NAME }}`
substitutions runners apply to workflow files. Secrets carry
ciphertext on the wire (NaCl sealed-box, gh-compatible); variables
are plaintext.

Scopes:

- `repo:read` on the read endpoints (including the public-key probe)
- `repo:write` on PUT / POST / PATCH / DELETE

The org variants live under `/orgs/{org}/actions/...` and follow the
same scope rules.

The user variants live under `/api/v1/user/actions/...`, store
personal workflow secrets for the authenticated PAT owner, and use the
same sealed-box request/response shapes. Writes may be denied when the
instance enforces the Pro-only personal Actions secrets gate.

Environment-scoped secrets can be managed from repository settings and
are part of the runner storage model, but they do not have a public REST
surface yet. Jobs that declare `environment: NAME` receive those secrets
only when the repository has a configured environment with the same
name; those values are still redacted and never appear in list/get
responses.

## Sealed-box (secrets only)

shithub never accepts plaintext secret values over REST. Clients
must encrypt with the server's X25519 public key first.

```
GET /api/v1/repos/{o}/{r}/actions/secrets/public-key
```

```json
{
  "key_id": "kIaP4w1eTJDhRoxw",
  "key":    "MCowBQYDK2VuAyEA..."
}
```

`key_id` is a stable identifier for the public key. Clients echo it
on the PUT body so the server can detect a stale local cache and
reject (with HTTP 422 `stale key_id`) rather than silently fail to
decrypt to garbage.

To encrypt:

```python
import base64, nacl.public
pub = nacl.public.PublicKey(base64.b64decode(key))
sealed = nacl.public.SealedBox(pub).encrypt(b"my-secret-value")
print(base64.b64encode(sealed).decode())
```

The Go-side equivalent:

```go
var pub [32]byte; copy(pub[:], pubKeyBytes)
ct, _ := box.SealAnonymous(nil, []byte("my-secret-value"), &pub, rand.Reader)
```

## Secrets endpoints

```
GET    /api/v1/repos/{owner}/{repo}/actions/secrets/public-key
GET    /api/v1/repos/{owner}/{repo}/actions/secrets
GET    /api/v1/repos/{owner}/{repo}/actions/secrets/{name}
PUT    /api/v1/repos/{owner}/{repo}/actions/secrets/{name}
DELETE /api/v1/repos/{owner}/{repo}/actions/secrets/{name}

GET    /api/v1/orgs/{org}/actions/secrets/public-key
GET    /api/v1/orgs/{org}/actions/secrets
GET    /api/v1/orgs/{org}/actions/secrets/{name}
PUT    /api/v1/orgs/{org}/actions/secrets/{name}
DELETE /api/v1/orgs/{org}/actions/secrets/{name}

GET    /api/v1/user/actions/secrets/public-key
GET    /api/v1/user/actions/secrets
GET    /api/v1/user/actions/secrets/{name}
PUT    /api/v1/user/actions/secrets/{name}
DELETE /api/v1/user/actions/secrets/{name}
```

### List + Get response

```json
[
  {
    "name":       "DEPLOY_TOKEN",
    "created_at": "2026-05-12T18:00:00Z",
    "updated_at": "2026-05-12T18:00:00Z"
  }
]
```

The list response **never** carries the value (plaintext or
ciphertext). This is identical to gh's behavior. To use a secret,
inject it via a workflow's `${{ secrets.NAME }}` reference.

### PUT body

```
PUT /api/v1/repos/alice/demo/actions/secrets/DEPLOY_TOKEN
Content-Type: application/json
```

```json
{
  "encrypted_value": "base64-of-sealed-box-output",
  "key_id":          "kIaP4w1eTJDhRoxw"
}
```

`204 No Content` on success. Errors:

| Status | Code-shaped meaning                                              |
|------:|-------------------------------------------------------------------|
| 400   | `encrypted_value` is not valid base64.                            |
| 422   | `encrypted_value` is empty, or `key_id` is stale, or the secret name is malformed (`^[A-Za-z_][A-Za-z0-9_]*$`, ≤100 chars). |
| 422   | Sealed-box decode failed (likely a stale local public-key cache). |
| 403   | PAT lacks `repo:write` (or org admin).                            |
| 503   | Operator did not configure the sealed-box keypair on the server.  |

Server-side: the decoded plaintext is re-encrypted with the shared
storage AEAD (`internal/auth/secretbox`) before INSERT. Plaintext
never lands in postgres.

## Variables endpoints

Variables are NOT secrets — they carry plaintext config and the
list/get endpoints return values directly. The runner exposes them
via `${{ vars.NAME }}`.

```
GET    /api/v1/repos/{owner}/{repo}/actions/variables
POST   /api/v1/repos/{owner}/{repo}/actions/variables
GET    /api/v1/repos/{owner}/{repo}/actions/variables/{name}
PATCH  /api/v1/repos/{owner}/{repo}/actions/variables/{name}
DELETE /api/v1/repos/{owner}/{repo}/actions/variables/{name}

GET    /api/v1/orgs/{org}/actions/variables
POST   /api/v1/orgs/{org}/actions/variables
GET    /api/v1/orgs/{org}/actions/variables/{name}
PATCH  /api/v1/orgs/{org}/actions/variables/{name}
DELETE /api/v1/orgs/{org}/actions/variables/{name}
```

### Create request

```json
{ "name": "API_URL", "value": "https://api.example" }
```

Returns the row shape with `created_at`/`updated_at`:

```json
{
  "name":       "API_URL",
  "value":      "https://api.example",
  "created_at": "2026-05-12T18:00:00Z",
  "updated_at": "2026-05-12T18:00:00Z"
}
```

PATCH accepts `{"value": "..."}` and returns the updated row.

Constraints:

- `name` matches `^[A-Za-z_][A-Za-z0-9_]*$` and is 1–100 chars.
- `value` is UTF-8 ≤4096 chars.

## Operator setup

The sealed-box keypair is operator-supplied via
`SHITHUB_ACTIONS__SECRETS__BOX_PRIVATE_KEY_B64` (base64 of a 32-byte
X25519 private key). Generate one with:

```sh
openssl rand -base64 32
```

When unset, the server generates a per-process keypair at startup
and logs a loud warning. Secrets PUT against one process won't be
decryptable by another — production deployments MUST configure
this knob.
