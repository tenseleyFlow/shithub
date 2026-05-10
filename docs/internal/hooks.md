# Git hooks

Bare-repo git hooks are how the push pipeline (S14) gets called.
shithub installs **pre-receive** (gates) and **post-receive** (enqueue)
on every repo. Both are tiny shell shims that exec into the same
`shithubd` binary the web/SSH/worker layers run.

## Why shell shims and not direct binary symlinks

git invokes hooks with stdin piped, a particular cwd, and a controlled
env. The shim normalizes the entry point: `exec /path/to/shithubd hook
<name>`. Hooks aren't symlinked because (a) macOS git treats some
symlinks oddly under hardened runtime, and (b) the shim explicitly
re-exec's so signals and exit codes propagate cleanly.

The shim is regenerated on every `hooks.Install` call — there's no
manual editing path, by design. If the binary moves (deploy upgrade,
versioned install path), run `shithubd hooks reinstall --all` to point
every repo at the new location.

## Hook execution flow

```
git push  ──▶  receive-pack
              │
              ▼
         pre-receive  (one process per push)
              │ stdin: "<old> <new> <ref>" lines
              │ env:   SHITHUB_USER_ID / _USERNAME / _REPO_ID / _REPO_FULL_NAME
              │        SHITHUB_PROTOCOL / _REMOTE_IP / _REQUEST_ID
              ▼
         exec shithubd hook pre-receive
              │ exit 0 → accept
              │ exit ≠ 0 → reject; stderr is shown to the pusher
              ▼
       (push proceeds)
              │
              ▼
         post-receive  (one process per push)
              │ stdin: same shape
              ▼
         exec shithubd hook post-receive
              │ INSERT push_events row per ref
              │ INSERT job (push:process)
              │ NOTIFY shithub_jobs
              │ exit 0
              ▼
       worker picks it up via LISTEN
```

In-browser file-editor commits do not execute git hooks: they are built
with plumbing inside the already-bare repository and advance the branch
with `git update-ref`. To keep downstream behavior identical to pushed
commits, `internal/repos/webedit` runs the same branch-protection
enforcer before the ref update, inserts `push_events.protocol = 'web'`
after a successful CAS, enqueues `push:process`, and sends
`NOTIFY shithub_jobs`.

## pre-receive contract

Implements the **minimum gates** described in S14. The full branch
protection engine is S20.

Re-checks the following from the DB even though the env carries them
(env can be stale on a long-lived SSH session):

* User not suspended (`users.suspended_at IS NULL`).
* Repo not archived (`repos.is_archived = false`).
* Repo not soft-deleted (`repos.deleted_at IS NULL`).

Failures emit a `shithub: ...` line on stderr that git surfaces directly
to the pusher's terminal. Latency budget: <100ms p99.

## post-receive contract

* Reads stdin, ignores empty/malformed lines.
* For each `<old> <new> <ref>` line: INSERT a `push_events` row with
  protocol `http` or `ssh`, then enqueue a `push:process` job carrying
  the new event id. Web-editor commits use the same table with protocol
  `web`.
* Issues a single `NOTIFY shithub_jobs` per push (workers wake on the
  next tx commit).
* Exits 0 even on internal errors. The push has already landed; the
  pipeline is async — partial enqueue failures surface in the worker
  logs and a backstop reconciler (post-MVP) can re-process orphaned
  events.

## Installation

* On repo create (`internal/repos/create.go::Create`): runs
  `hooks.Install` after `RepoFS.InitBare` succeeds. The S11 plumbing
  initial commit does *not* fire hooks — that's correct, the contract
  is hooks fire on user-driven pushes only.
* On deploy: `shithubd hooks reinstall --all` walks every active repo
  via the DB and re-installs. Single repos: `--repo owner/name`.
* The binary path baked into the shim is `os.Executable()` of the
  running shithubd at install time, resolved through `filepath.Abs`.
  Test fixtures that don't exercise hooks pass `ShithubdPath: ""` to
  `repos.Create.Deps` to skip installation.

## Failure modes worth knowing

* **`SHITHUB_*` env not set** (e.g. someone manually triggered a push
  via a path that bypassed S12/S13): pre-receive returns the "missing
  context" error. post-receive logs a warning and exits 0 — the push
  still lands.
* **DB unreachable from hook**: pre-receive returns "server error";
  the user sees a generic message, the push aborts. post-receive logs
  and exits 0; backstop reconciler will pick up the unprocessed push
  on next opportunity.
* **Binary path drift between install and execution**: the shim's hard-
  coded path no longer exists. git will report "hook execution failed"
  and abort the push. Operators recover with `hooks reinstall`.
* **Stale shim from previous shithubd version**: `Install` is
  idempotent and overwrites; re-running on a deploy is the right move.

## Deferred: hook DB role split

S14's spec calls for the hook to connect with a least-privilege Postgres
role distinct from the one the web server uses. S14 ships the dev path
(single role) and defers the split to S37 deploy automation. The full
GRANT recipe and the planned config plumbing live in
[`db-roles.md`](./db-roles.md), and the bullet is on the S37
deliverables list so it doesn't fall off.
