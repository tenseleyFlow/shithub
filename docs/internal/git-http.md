# Smart-HTTP git protocol

S12 ships `git clone`, `git fetch`, and `git push` over HTTPS. Authentication is HTTP Basic with either a password or a personal access token. The handlers shell out to canonical `git`'s `upload-pack` / `receive-pack` plumbing — no protocol re-implementation. Streams pack data without buffering. Cancels propagate to the subprocess so a closed client doesn't leave git running.

## What's wired

- `internal/git/protocol/pktline.go` — minimal pkt-line writer used to prepend the `# service=...` header to the info/refs response.
- `internal/git/protocol/exec.go` — `Cmd(ctx, svc, gitDir, advertiseRefs, env)` builds the `*exec.Cmd` for `git-{upload,receive}-pack --stateless-rpc [--advertise-refs] <repo>`. `DrainStderr` runs a goroutine that copies stderr into a 16 KiB capped buffer; the OS pipe never fills, the caller's stdout copy never deadlocks. `WaitDelay = 250ms` so a stuck subprocess doesn't pin a worker after ctx cancel.
- `internal/web/handlers/githttp/auth.go` — `resolveBasicAuth(ctx, header)` parses HTTP Basic, prefers the PAT path when the secret carries the canonical `shithub_pat_` prefix, and falls back to argon2id password verification. Constant-time discipline: missing username runs `password.VerifyAgainstDummy` so timing doesn't leak existence.
- `internal/web/handlers/githttp/handler.go` — the four route bodies + inline owner-only authorization. `MountSmartHTTP(r)` registers them on a CSRF/timeout/compression-exempt route group.
- `internal/web/githttp_wiring.go` — `buildGitHTTPHandlers` constructs the handler set from cfg + pool.
- `internal/web/handlers/handlers.go` — `GitHTTPMounter` Dep field; the route group sits alongside the static / CSRF-protected groups.
- `internal/web/server.go` — Compress + Timeout no longer global; they're applied per-group in handlers.go so the git group can stream uncompressed for many minutes.

## Routes

All four are registered without CSRF, without response compression, and without the global request timeout. Request bodies on the POST endpoints are capped at `Deps.MaxPushBytes` (default 2 GiB) via `http.MaxBytesReader`.

| Route | Method | Notes |
|---|---|---|
| `/{owner}/{repo}.git/info/refs?service=git-upload-pack` | GET | public allowed; private 401 with `WWW-Authenticate: Basic realm="shithub"` |
| `/{owner}/{repo}.git/info/refs?service=git-receive-pack` | GET | always 401 if anon; otherwise inline owner-only check |
| `/{owner}/{repo}.git/git-upload-pack` | POST | streams `req.Body` → upload-pack stdin → `w` |
| `/{owner}/{repo}.git/git-receive-pack` | POST | same shape; sets `SHITHUB_*` env vars on the subprocess (S14 hooks consume them) |

## Auth shape

```
                       header == ""
                            │
                            ▼
                       Anonymous=true ──┐
                                        │
header has Basic creds → decode user:secret
                            │
                  secret has "shithub_pat_" prefix?
                  ┌───────────yes────────────┐    no
                  ▼                          ▼     │
            resolveViaPAT             resolveViaPassword
                  │                          │     │
              found?                  username found? & pw ok?
              ┌─────yes──┐                  ┌─────yes──┐
              ▼          ▼                  ▼          ▼
        ResolvedAuth   fall              ResolvedAuth  errBadCredentials
                       through to                      (constant-time discipline:
                       password path                    runs VerifyAgainstDummy
                                                        when user lookup fails)
```

The ResolvedAuth result carries `UserID`, `Username`, and `ViaPAT`. Anonymous resolves only when the header is missing entirely — bad credentials always become `errBadCredentials` regardless of which path failed, so callers can't probe for valid usernames or distinguish "wrong password" from "non-existent user."

## Permissions (S15 will refactor)

Inline V1 rules:

- **Read** (upload-pack):
  - public repo → anonymous OK; auth'd user OK as long as not suspended.
  - private repo → must be the owner.
- **Write** (receive-pack):
  - must be authenticated.
  - must be the owner.
  - repo must NOT be archived (we surface "repository is archived; pushes are disabled" so `git push`'s stderr shows it).
  - repo must NOT be soft-deleted (410 Gone).

S15 lands `policy.Can` — this whole block becomes a single function call.

## Subprocess lifecycle

```
http handler                  protocol.Cmd                git
─────────────────────────────────────────────────────────────────
authorize                     ─────────────────► CommandContext (ctx-bound)
                              ◄──────────────────── *exec.Cmd
DrainStderr(cmd)               ─────────────────► StderrPipe + goroutine
cmd.Stdout = w                                         │
cmd.Stdin  = req.Body                                  │
cmd.Run                       ─────────────────► fork + exec git
                              ◄──────────────────── stdout streamed
                              (stderr drained to bounded buf in goroutine)
                              ◄──────────────────── exit
ctx cancel anywhere here       ─────────────────► cmd.Cancel = SIGKILL
                                                  WaitDelay 250ms
```

Why we drain stderr: when git-receive-pack writes lots of stderr (failed hook, push to non-existent ref, etc.), an undrained pipe fills the OS buffer (~64 KiB) and the subprocess blocks on writes — and our `cmd.Run` never returns. The goroutine + capped buffer prevents that.

Why bounded buffer: a malicious or pathological subprocess could otherwise OOM us by spamming stderr. We accept-and-drop after 16 KiB. The captured bytes are surfaced via the closure returned by `DrainStderr` for logging.

Why `WaitDelay`: ctx cancel sends SIGKILL, but the subprocess might still be writing pack bytes to a held pipe. Without `WaitDelay`, `cmd.Wait` waits indefinitely. 250 ms is enough for clean exit and snappy enough that a worker doesn't get pinned after a client disconnect.

## Hook environment

The receive-pack subprocess gets these env vars so S14's post-receive hook can identify the pusher:

| Var | Value |
|---|---|
| `SHITHUB_USER_ID` | numeric users.id of the auth'd user |
| `SHITHUB_USERNAME` | the user's lowercase username |
| `SHITHUB_REPO_ID` | numeric repos.id |
| `SHITHUB_REPO_FULL_NAME` | `<owner>/<repo>` |
| `SHITHUB_PROTOCOL` | `http` (S13 adds `ssh`) |
| `SHITHUB_REMOTE_IP` | client IP from RealIP middleware |
| `SHITHUB_REQUEST_ID` | request ID from RequestID middleware |
| `PATH` | inherited from the parent process |

Git propagates the parent environment to its hook subprocesses, so the post-receive script can `echo "$SHITHUB_USER_ID"` and get the right value.

`PATH` is included explicitly so `git` itself can find its sub-helpers when the parent process's PATH is the only sane source.

## Body cap

`http.MaxBytesReader(w, r.Body, MaxPushBytes)` wraps the request body before it reaches the subprocess. Default cap is 2 GiB (configurable via `Deps.MaxPushBytes`). When the cap is exceeded the next read errors and the subprocess sees stdin EOF; receive-pack returns a non-zero exit and we log it. From the client's POV the `git push` fails. This is good enough for V1; S36 will add per-repo overrides.

## Streaming

The handler writes the `Content-Type` + cache headers BEFORE `cmd.Run`. After that, every byte git writes to stdout goes straight to the response writer — Go's `http.ResponseWriter` flushes by default on Write when the body is not chunked-encoded. `chi`'s default `ResponseWriter` implements `http.Flusher`; manual verification via `tcpdump` was done in dev to confirm bytes leave the server in real time.

## Tests

`internal/web/handlers/githttp/githttp_test.go` — five end-to-end scenarios against a real `git` CLI:

- `TestGitHTTP_AnonClonePublic` — anon clone of a public repo with one initial commit succeeds; `rev-list --count HEAD = 1`.
- `TestGitHTTP_AnonClonePrivateFails` — anon clone of a private repo fails (with `GIT_TERMINAL_PROMPT=0` so we don't hang on the credential prompt).
- `TestGitHTTP_PATClonePrivate` — clone with the PAT in the URL userinfo succeeds.
- `TestGitHTTP_PATPushRoundtrip` — clone, commit, push to private repo with PAT, re-clone elsewhere, verify the new commit is visible.
- `TestGitHTTP_PushToArchivedRejected` — archive the repo, attempt push, expect non-zero exit AND "archived" in stderr.

`internal/git/protocol/protocol_test.go` — pkt-line format, over-length rejection, service advertisement shape, and a context-cancel test that asserts the subprocess dies within 5 seconds.

Tests skip cleanly when `SHITHUB_TEST_DATABASE_URL` is unset (matches the rest of the suite).

## Pitfalls / what to remember

- **Never reach for global Compress / Timeout.** They're per-group now. If you add a new top-level route group, copy the `r.Use(middleware.Compress)` + `r.Use(middleware.Timeout(...))` pair from the static / app groups.
- **Never log the Authorization header.** S08's redaction is wired globally in the access log; cross-test on git routes if you change the logging stack.
- **Username in HTTP Basic is informational.** git's credential helper passes whatever the user typed at the "Username for shithub:" prompt. We DON'T require it match the resolved user — for PATs the username is irrelevant, for password it's just the lookup key. Document this in user-facing help when we ship credential setup docs.
- **Stderr drain is required, not optional.** A bug that disables the drainer eventually deadlocks under load. The bounded buffer is the second backstop.
- **Body cap is on the receive-pack side only — but enforced before we open the subprocess.** Don't skip the LimitReader because "git will reject malformed packs anyway" — that's true but the malicious payload already hit RAM by then.
- **Clones of empty repos work.** info/refs returns just the service-advertisement preamble (`001e# service=...\n0000`) when the repo has zero refs; git handles the empty case fine.

## Open follow-ups (deferred)

- **Smart-SSH (S13)** reuses the auth/permission shape; the protocol-level differences are in the transport layer, not the authz layer.
- **Push processing pipeline (S14)** consumes the env vars set here. Today the receive-pack subprocess's hooks dir is empty; S14 wires post-receive.
- **Branch protection (S20)** lands as a pre-receive hook installed by S14.
- **LFS** is post-MVP.
- **Performance / large packs (S36)** will profile the streaming path once we have real-world push sizes.

## Related docs

- `docs/internal/storage.md` — RepoFS layout that produces the bare-repo path.
- `docs/internal/repo-create.md` — S11's repo creation flow whose output we're serving.
- `docs/internal/auth.md` — sessions + recent-2FA gate (irrelevant here; git over HTTP doesn't use sessions).
- `docs/internal/tokens.md` — PAT issuance and the hash format.
