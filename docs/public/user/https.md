# Cloning over HTTPS with a PAT

For git over HTTPS, you authenticate with a **personal access
token** (PAT), not your account password. This matches GitHub's
behavior since 2021 and reflects the same security thinking:
account passwords are more sensitive than scoped, revocable
tokens.

## 1. Create a PAT

Settings → Developer settings → Personal access tokens → "New
token".

- **Note** — name the token after where you'll use it ("laptop",
  "ci-runner-1"). Future-you will thank present-you.
- **Expiration** — pick the shortest interval that's tolerable.
  Tokens you forget about are tokens an attacker eventually finds.
- **Scopes** — for git push/pull from a workstation, pick `repo`
  (read+write). For read-only mirroring, `repo:read` is enough.

When you submit, the token is shown **once**. Copy it immediately
into your password manager — we never display it again.

## 2. Clone

```sh
git clone https://shithub.example/<owner>/<repo>.git
```

When git asks for credentials:

- **Username:** your shithub username.
- **Password:** the PAT.

## 3. Cache credentials

Typing the PAT every push gets old. Use a credential helper:

- **macOS:** `git config --global credential.helper osxkeychain`
- **Windows:** `git config --global credential.helper manager`
- **Linux (GNOME):** `git config --global credential.helper
  /usr/share/doc/git/contrib/credential/libsecret/...`
- **Anywhere, in a pinch:** `git config --global credential.helper
  cache` (in-memory, default 15-minute TTL).

The helper stores `(url, username, password)`; the next push to
the same host reuses it.

## 4. Use a CI runner

In CI, set the username/password as secrets and inject them via
the URL or `~/.netrc`. Use a token with the narrowest scope the
job needs and a short expiration.

```sh
git clone https://x-access-token:${SHITHUB_PAT}@shithub.example/owner/repo.git
```

Because the token is in the URL, make sure your CI doesn't echo
the URL into logs.

## When pushes fail

| Symptom                                              | Likely cause                              |
|------------------------------------------------------|-------------------------------------------|
| `403 Forbidden` on push                              | Token lacks `repo` write scope.           |
| `401 Unauthorized` immediately                       | Wrong username, expired token, or the token was revoked. |
| `protected branch hook declined`                     | Branch protection requires PR + reviews — push to a feature branch instead. |
| `pre-receive hook declined: repo over quota`        | Repo size cap hit; see your operator.     |
| `error: failed to push some refs … updates were rejected` | Standard git non-fast-forward — pull/rebase first. |
