# Storage

shithub has two storage layers:

1. **Object storage** — S3-compatible (MinIO in dev/test, DigitalOcean Spaces in prod). Used for avatars, attachments, and (post-MVP) LFS objects. The production bucket is DigitalOcean Spaces; the `s3` naming reflects the compatible API, not AWS.
2. **Repo filesystem storage** — bare git repositories on a local block-storage volume, in a sharded layout owned by the `RepoFS` helper.

Both layers live behind the package `internal/infra/storage`. Path validation is the **security boundary** — every entry that takes user-supplied owner/repo names goes through `RepoPath`, which rejects unsafe inputs against a strict whitelist. If repo paths can be tricked, every later sprint inherits the bug; the test suite *over*-tests this.

## Object storage

### Interface

```go
type ObjectStore interface {
    Put(ctx, key, body io.Reader, opts PutOpts) (PutResult, error)
    Get(ctx, key) (io.ReadCloser, ObjectMeta, error)
    Stat(ctx, key) (ObjectMeta, error)
    Delete(ctx, key) error
    List(ctx, prefix, opts ListOpts) (ListResult, error)
    SignedURL(ctx, key, ttl, method) (string, error)
}
```

Two implementations:

- `S3Store` — backed by minio-go. Works against any S3-compatible endpoint. `force_path_style=true` for MinIO; `false` for Spaces.
- `MemoryStore` — in-process map for tests. Honors the same semantics including `If-None-Match`.

### Bucket / key scheme

Single bucket per environment: `shithub-dev`, `shithub-staging`, `shithub-prod`. In production this is a DigitalOcean Spaces bucket configured through the S3-compatible client. Per-scope key prefixes ease policy and tenant isolation:

```
lfs/<owner>/<repo>/<sha256>           # LFS objects (post-MVP, key shape reserved)
attachments/<scope>/<id>/<filename>   # issue/PR/comment attachments
avatars/<user_id>/<hash>.png          # largest rendered avatar variant
avatars/<user_id>/<hash>-<size>.png   # smaller rendered avatar variants
avatars/orgs/<org_id>/<hash>.png      # largest rendered org avatar variant
avatars/orgs/<org_id>/<hash>-<size>.png
actions/runs/<run_id>/...             # Actions logs + artifacts
backups/...                           # S37
```

Avatar uploads are decoded from PNG, JPEG, or GIF and re-encoded to PNG before storage. Keys are always lowercase.

### Semantics worth knowing

- **Idempotent delete.** `Delete` returns nil for absent keys.
- **`If-None-Match: "*"`** is the only precondition supported. Causes `Put` to fail with `ErrPreconditionFailed` when the destination already exists. Used to avoid overwrite races.
- **`SignedURL`** supports `GET` and `PUT` only (no multipart). Avatar/attachment direct-uploads in later sprints rely on this; we wire it now even though no caller uses it yet.
- **`List` with `Recursive=false`** uses `/` as a delimiter and surfaces folders in `CommonPrefixes` — matches S3 behavior.
- **`ContentLength`** in `PutOpts` is a hint; pass 0 to let the backend buffer/stream.

### MinIO vs Spaces drift

The two backends share an interface but behavioral edges differ:

- **Path-style addressing.** MinIO needs `force_path_style=true`. Spaces supports virtual-host-style (the default).
- **Lifecycle rules.** Spaces and MinIO honor different subsets of the S3 lifecycle XML. Apply rules through their respective consoles, not via the SDK. The production Actions prefix uses `deploy/spaces/actions-lifecycle.json` (`actions/runs/`, 90-day expiry).
- **ACL semantics.** Spaces supports `public-read` on objects; MinIO uses bucket policies. We don't rely on either today (all reads go through the app).
- **Listing pagination.** Both honor `MaxKeys` + continuation tokens, but the page sizes they prefer differ. Don't assume an exact count per page.

Run the integration tests against both backends periodically. Document new gotchas here as they surface.

## Repo filesystem storage

### Layout

```
<root>/
  <shard>/        # first 2 chars of lowercased owner ('_'-padded if shorter)
    <owner>/
      <name>.git
```

Two-character shard gives 1296 buckets — enough that no shard exceeds tens of thousands of subdirectories at our scale. Reversible (the shard is *derived*, not stored separately) and debuggable. We deliberately avoid hash-based sharding because it scatters related entries.

`<root>` defaults to `/data/repos` and MUST live on the production block-storage volume — NOT the droplet root disk. The root disk is small and resets on droplet rebuilds.

### Path validation rules (the security boundary)

Owner and repo names must match `^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`:

- Lowercase ASCII letters, digits, hyphens only.
- Cannot start or end with `-`.
- Length 1..39 (matches GitHub username constraint).
- No `..`, no leading `.`, no `/`, no absolute paths, no whitespace, no NUL bytes.
- Display casing is a DB concern; path casing is normalized to lowercase here.

Anything that fails the whitelist returns `ErrInvalidPath` with a precise reason. `RepoFS.Delete` and `RepoFS.Move` additionally guard against paths that resolve outside `<root>` (`ErrEscapesRoot`).

### Default branch

Every `git init --bare` invoked through `InitBare` uses `--initial-branch=trunk`. There is no path through this package that creates a bare repo with a different branch. Verified via `git symbolic-ref HEAD` returning `refs/heads/trunk` in `TestInitBare_HEADIsTrunk`.

### Atomic operations

`WriteAtomic(path, src)` writes to a tempfile (`.<basename>.tmp.<hex>`) in the **same directory**, fsyncs, then renames. A crash between write and rename leaves the temp file behind (callers may sweep these on startup) but never a partial file at the destination. `<root>` and any temp dir used for atomic ops MUST live on the same mount — `/data/repos/` and the temp space both live under `/data/`.

`Move(old, new)` refuses to overwrite an existing destination, returning `ErrAlreadyExists`. This avoids silent corruption on concurrent moves; the loser surfaces a clear error.

### Future: symlinks inside repos

When tooling lands that walks repo *contents* (S17 code tab, S37 backup), it MUST use `O_NOFOLLOW` or equivalent to avoid traversing symlinks out of the repo. No content traversal happens in S04, but the constraint is captured here.

## Configuration

All storage settings flow through `internal/infra/config` (see `docs/internal/config.md`):

| Key | Type | Default | Notes |
|---|---|---|---|
| `storage.repos_root` | string | `/data/repos` | Filesystem root for bare repos. Required. |
| `storage.s3.endpoint` | string | `""` | Host[:port], no scheme. Empty disables object storage. Production uses the DigitalOcean Spaces endpoint. |
| `storage.s3.region` | string | `us-east-1` | Region for SigV4 signing. |
| `storage.s3.access_key_id` | string | `""` | |
| `storage.s3.secret_access_key` | string | `""` | Redacted by `config print`. |
| `storage.s3.bucket` | string | `""` | Single bucket per environment. |
| `storage.s3.use_ssl` | bool | `false` | True for Spaces, false for local MinIO. |
| `storage.s3.force_path_style` | bool | `true` | True for MinIO, false for Spaces. |

If any S3 field is set, **all** required fields (endpoint, bucket, access_key_id, secret_access_key) must be set — `Validate` rejects partial configuration.

## Operational helpers

### `shithubd storage check`

Exits 0 when:

1. `storage.repos_root` exists, is a directory, and is writable (verified by creating + removing a probe file).
2. PUT and GET round-trip successfully against the configured S3 bucket.

When the S3 block is unconfigured, only (1) is checked — output makes the skip explicit. Used in deploy smoke tests and from operator terminals.

```sh
make storage-check
# or:
./bin/shithubd storage check
```

### `make dev-storage` / `make dev-storage-down` / `make dev-storage-reset`

Brings up MinIO via docker-compose, seeds the `shithub-dev` bucket via the `minio-init` one-shot, and prints the API/console URLs. Credentials are **non-default** even in dev — MinIO's defaults (minioadmin/minioadmin) are insecure.

```sh
make dev-storage
# MinIO S3 API: http://127.0.0.1:9000  console: http://127.0.0.1:9001
# Credentials: shithub-dev / shithub-dev-secret-please-change
```

## Quotas

`Quota{Used, Limit}` is the small arithmetic helper for byte budgets.
`Limit == 0` means unlimited. `WouldExceed(n)` and `Available()` give
callers a uniform interface.

PAYMENTS SP08 adds organization storage quotas through the billing and
entitlements domains. Org-owned git pushes enforce storage in the
pre-receive hook: the hook measures the actual bare repo directory,
adjusts the org's recalculated usage counters for that repo, and rejects
pushes that would exceed the effective quota. `RepoFS.DiskUsageBytes`
is the shared source-size helper used by both the hook and
`repo:size_recalc`.

Object upload quota enforcement is tracked separately; callers should
use `internal/entitlements.CheckOrgStorageQuota` once their write path
knows the owner organization and incoming byte count.

## Testing

- **Unit tests** (`*_test.go`) run with `go test ./internal/infra/storage/...` — no external dependencies.
  - Path-validation table covers `..`, absolute paths, leading/trailing dash, dotfiles, uppercase, unicode, length, NUL/newline, slash, punctuation.
  - WriteAtomic crash-survival via fault-injection reader — destination must not exist after a partial write, and no temp file may leak.
  - InitBare verifies `HEAD` resolves to `refs/heads/trunk`.
  - Memory store covers Put/Get/Stat/Delete/List (recursive + delimited)/SignedURL/IfNoneMatch/large-body round-trip.
- **S3 integration tests** are in `s3_test.go` and gate on `SHITHUB_TEST_S3_ENDPOINT` (and the matching credentials). They skip cleanly when the env var is empty. CI sets these via the MinIO compose service.

```sh
SHITHUB_TEST_S3_ENDPOINT=127.0.0.1:9000 \
SHITHUB_TEST_S3_ACCESS_KEY_ID=shithub-dev \
SHITHUB_TEST_S3_SECRET_ACCESS_KEY=shithub-dev-secret-please-change \
SHITHUB_TEST_S3_BUCKET=shithub-dev \
go test ./internal/infra/storage/...
```

## Related docs

- `docs/internal/config.md` — configuration loader and env var conventions.
- `docs/internal/observability.md` — metrics around storage will land in S14 (push pipeline).
