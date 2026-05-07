# SSH git protocol

S13 lights up `git clone git@host:owner/repo.git`, `git fetch`, and `git push` over SSH. The S07 `ssh-shell` placeholder is now the real dispatcher: it parses `SSH_ORIGINAL_COMMAND`, resolves user + repo against the DB, runs an inline owner-only authz, sets the `SHITHUB_*` hook environment, closes the DB pool, and `syscall.Exec`s the canonical `git-upload-pack` or `git-receive-pack` binary. After exec, sshd's stdin/stdout/stderr flow directly to git.

## What's wired

- `internal/git/protocol/ssh_command.go` — `ParseSSHCommand` strict-parses `SSH_ORIGINAL_COMMAND` into `Service + Owner + Repo`. Whitelist regex; matched-quote alternation (`'a/b'` or `a/b` only — `a/b'` rejected); single trailing `.git` stripped; owner + repo run through the storage-shape rules.
- `internal/git/protocol/ssh_dispatch.go` — `PrepareDispatch` does the DB + authz + env work. Returns the binary path, argv, and env vector the caller needs to exec git. Also exposes `FriendlyMessageFor(err, requestID)` so the caller writes a consistent stderr line and `ParseRemoteIP(SSH_CONNECTION)` for the env var.
- `cmd/shithubd/ssh.go::sshShellCmd` — the cobra command sshd invokes. Replaces the S07 stub. Manages the DB pool lifecycle and calls `syscall.Exec`.

The `ssh-authkeys` flow from S07 (the `AuthorizedKeysCommand` handler) is unchanged; it still emits the `command="shithubd ssh-shell <user_id>",no-pty,…` line, which is what binds an inbound SSH connection to a known user before this dispatcher ever runs.

## End-to-end flow

```
client                          sshd                       shithubd
──────────────────────────────────────────────────────────────────────
ssh git@host                    ───►
git-upload-pack 'alice/foo.git'
                                ssh-authkeys SHA256:…
                                ◄─── command="shithubd ssh-shell 42",no-pty,…
                                ───── pubkey auth OK
                                fork; setenv SSH_ORIGINAL_COMMAND, SSH_CONNECTION
                                exec shithubd ssh-shell 42
                                                            │
                                                            ├─ ParseSSHCommand
                                                            ├─ DB pool (max 2)
                                                            ├─ GetUserByID, GetUserByUsername, GetRepoByOwnerUserAndName
                                                            ├─ Inline authz
                                                            ├─ Build SHITHUB_* env
                                                            ├─ pool.Close()
                                                            └─ syscall.Exec git-upload-pack /data/repos/al/alice/foo.git
                                                                              │
                                                                              ▼
                                                                       git pack protocol
                                                                       (sshd's stdio is now git's)
```

## Strict command parsing

The whitelist regex is intentionally narrow:

```
^(git-upload-pack|git-receive-pack)\s+(?:'([^']+)'|([^'\s]+))$
```

The path group is one of two alternatives — quoted-with-matching-quotes OR no-quotes-and-no-spaces. Anything else (mismatched quotes, escaped quotes, command chaining, extra whitespace, alternate services like `git-archive`) returns `ErrUnknownSSHCommand` and the dispatcher writes "shithub does not allow shell access" to stderr.

After parse, the path goes through:

1. Strip a single trailing `.git`.
2. Split on the first `/`.
3. Lowercase both halves (the storage layer is lowercase-only).
4. Validate `owner` against `^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$` (S05 username shape).
5. Validate `repo` against `^[a-z0-9](?:[a-z0-9._-]{0,98}[a-z0-9_])?$` (S11 repo shape).
6. Reject if either contains `..`, leading dot, or extra slashes.

These validators live inside the `protocol` package rather than importing `internal/infra/storage` so that an SSH connection — which runs a brand-new shithubd process every time — doesn't pay the storage package's init cost.

## Authorization (S15 will refactor)

Inline V1 rules, mirroring the HTTP path:

- **upload-pack**:
  - public repo → any non-suspended user.
  - private repo → must be the owner; non-owners get the not-found message (no existence leak).
- **receive-pack**:
  - must be the owner.
  - repo not archived (else `MsgArchived`).
  - repo not soft-deleted (else `MsgRepoNotFound`).

Suspended or soft-deleted users are rejected up-front with `MsgSuspended`.

S15 lifts these into `policy.Can(actor, action, resource)`.

## Hook environment

Same as HTTP, plus protocol distinction:

| Var | Value |
|---|---|
| `SHITHUB_USER_ID` | numeric `users.id` of the authenticated client |
| `SHITHUB_USERNAME` | the user's lowercase username |
| `SHITHUB_REPO_ID` | numeric `repos.id` |
| `SHITHUB_REPO_FULL_NAME` | `<owner>/<repo>` (lowercased) |
| `SHITHUB_PROTOCOL` | `ssh` |
| `SHITHUB_REMOTE_IP` | first field of `SSH_CONNECTION` |
| `SHITHUB_REQUEST_ID` | freshly generated 16-byte hex token |
| `PATH` | inherited from the parent process so git's helpers resolve |

Because git propagates its environment to its hook subprocesses, S14's post-receive can `echo "$SHITHUB_USER_ID"` and get the right value.

`PATH` is included so `git-receive-pack` can find `git-pack-objects`, `git-index-pack`, and friends.

## syscall.Exec discipline

Three things are non-negotiable here:

1. **Close the DB pool BEFORE `syscall.Exec`.** Go's `defer` does NOT fire on `syscall.Exec` — the OS replaces the current process image and the runtime never returns. The pgx pool's TCP connections to Postgres would otherwise leak into git's FD table. We close explicitly.
2. **Resolve the binary via `exec.LookPath`.** `syscall.Exec` requires an absolute path. `exec.LookPath("git-upload-pack")` walks `$PATH` and returns the first match. If git isn't on the daemon user's `$PATH`, the operator sees `shithub: server misconfigured` in their git client.
3. **No writes to stdout AFTER the exec call.** If the exec call returns, it failed (typically EACCES on the binary or ENOENT). We write a friendly error and exit non-zero. On success we never see another instruction in this process.

`sysExec` is a package-level var pointing at `syscall.Exec` so unit tests can stub it. The production binding takes the `//nolint:gosec` for G204 because the inputs are constrained: `bin` is the LookPath result of a fixed service name, `argv[1]` is a sanitized path from `storage.RepoFS`.

## Friendly error catalogue

| Error sentinel | Stderr line |
|---|---|
| `ErrSSHRepoNotFound` | `shithub: repository not found` |
| `ErrSSHPermDenied` | `shithub: permission denied` |
| `ErrSSHArchived` | `shithub: this repository is archived; pushes are disabled` |
| `ErrSSHSuspended` | `shithub: your account is suspended` |
| `ErrUnknownSSHCommand` | `shithub does not allow shell access` |
| `ErrInvalidSSHPath` | `shithub: repository not found` (collapses to the same message — bad paths are indistinguishable from missing repos) |
| (anything else) | `shithub: internal error (request_id=<hex>)` |

The friendly message is what the user sees in `git`'s output. The structured slog event is what the operator reads — it logs at `WARN` for denials and `INFO` for successful dispatches with `(user_id, op, owner, repo, remote_ip)` fields.

## Tests

`internal/git/protocol/ssh_command_test.go` — accepts (quoted, unquoted, dotted names, hyphenated names, `.git` suffix), rejects (wrong service, mismatched quotes, command injection attempts, leading/trailing whitespace, absolute paths, dot-dot, leading dot in repo name).

`internal/git/protocol/ssh_dispatch_test.go` (DB-integration):
- `TestDispatch_PublicCloneByOwner` — full round-trip, asserts on argv path + env vars.
- `TestDispatch_PublicCloneByOther` — non-owner pull of public repo OK.
- `TestDispatch_PrivateCloneByOtherIsNotFound` — non-owner sees `ErrSSHRepoNotFound`.
- `TestDispatch_PushByNonOwnerIsPermDenied` — non-owner push → `ErrSSHPermDenied`.
- `TestDispatch_PushToArchivedIsArchived`.
- `TestDispatch_SuspendedUserSuspended`.
- `TestDispatch_UnknownCommandIsRejected`.
- `TestFriendlyMessageFor` covers the message catalogue.
- `TestParseRemoteIP` covers `SSH_CONNECTION` parsing edge cases.

We don't unit-test the cobra command's `syscall.Exec` path; the whole point of `syscall.Exec` is that the test process would be replaced. The dispatcher tests cover everything up to the exec call; the deploy doc has a manual smoke-test recipe.

## Deploy checklist

Reviewed once before going live:

- The shithubd binary runs as a dedicated `shithub-ssh` system user.
- That user has read+write on `/data/repos/`.
- That user's `$PATH` includes `git-upload-pack` and `git-receive-pack` (typically `/usr/lib/git-core/`).
- sshd's `AuthorizedKeysCommand` points at `shithubd ssh-authkeys`.
- sshd's `AuthorizedKeysCommandUser` is set to `shithub-ssh` (NOT root).
- The `command=` directive in the AKC line forces `shithubd ssh-shell <user_id>` — a user can't bypass it via `~/.ssh/authorized_keys` because the AKC is the only key source we trust.

Failure mode for missing config: any unset cfg value (`cfg.DB.URL`, `cfg.Storage.ReposRoot`) lights up `shithub: server misconfigured` in the user's git client. Operator reads the structured slog line on the daemon side.

## Pitfalls / what to remember

- **Strict command parsing is the security boundary.** Don't relax the regex without writing a fresh fixture set. Shell injection attempts are caught here, not later.
- **Path validation is the second boundary.** `..`, absolute paths, and embedded slashes are caught BEFORE we ever construct a filesystem path.
- **`syscall.Exec` doesn't run defers.** Anything that needs cleanup runs explicitly before the exec call. Today that's just the pool; if you add a tempfile or a flock, close it first.
- **Stdin/stdout flow directly to git after exec.** sshd doesn't insert a layer — what git writes is what the client reads. No buffering hooks possible after this point; if you need to inspect bytes, do it before exec.
- **No environment leak.** We replace the entire env with the curated `SHITHUB_*` set + `PATH`. `SSH_CONNECTION`, `SSH_ORIGINAL_COMMAND`, etc. are not propagated to git.
- **Concurrent pushes serialize on `refs/...lock`.** Multiple SSH connections pushing to the same ref take turns; different refs go in parallel. This is git's own behavior, not ours.

## Open follow-ups (deferred)

- **SSH certificates / CA-signed user keys** are post-MVP — today it's strictly key-by-key.
- **`git-lfs` over SSH** is post-MVP; the dispatcher rejects `git-lfs-authenticate` (and anything else) as unknown.
- **Pull-mirror / federation** is post-MVP.
- **S14** wires post-receive hooks; this dispatcher is the source of the env vars they read.
- **S15** refactors the inline owner-only check into `policy.Can`. Both this dispatcher and the HTTP handler swap atomically.

## Related docs

- `docs/internal/git-http.md` — the parallel HTTP transport, same auth/permission shape.
- `docs/internal/repo-create.md` — repo creation flow whose output we serve.
- `docs/internal/storage.md` — `RepoFS` layout that produces the bare-repo path.
