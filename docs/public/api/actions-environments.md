# Actions Environments API

Repository environment endpoints are PAT-authenticated and require repository
write access for mutations. Read routes follow repository visibility: public
repositories are readable by anonymous clients, while private repositories
require repository read permission.

## Environments

```text
GET /api/v1/repos/{owner}/{repo}/environments
```

Lists repository environments with deployment protection settings and secret
counts.

```text
GET /api/v1/repos/{owner}/{repo}/environments/{environment}
```

Returns a single environment, including wait timer, reviewer count,
deployment-branch policy, custom branch patterns, and secret metadata.

```text
PUT /api/v1/repos/{owner}/{repo}/environments/{environment}
DELETE /api/v1/repos/{owner}/{repo}/environments/{environment}
```

Creates, updates, or deletes a repository environment. The request body for
`PUT` accepts wait timer, reviewer, self-review, and branch-policy fields that
match the repository settings UI.

## Environment Secrets

```text
GET /api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/public-key
```

Returns the repository public key used to encrypt environment secrets.

```text
GET /api/v1/repos/{owner}/{repo}/environments/{environment}/secrets
```

Lists environment secret metadata. Secret values are never returned.

```text
GET /api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/{name}
PUT /api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/{name}
DELETE /api/v1/repos/{owner}/{repo}/environments/{environment}/secrets/{name}
```

Reads, upserts, or deletes a single environment secret. `PUT` expects an
encrypted value and key id from the public-key endpoint.
