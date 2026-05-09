# Quickstart

This walks the simplest possible path: sign up, create a repo,
clone it, push a commit. About five minutes.

## 1. Sign up

Visit `/signup`. You'll need:

- A working email address (a verification link is sent there).
- A username — letters, numbers, and dashes; not in our reserved
  list (the form tells you if it is).
- A password — 12 characters minimum and not in the common-password
  list. Argon2id is used to hash; we never store the plaintext.

After you submit, the verification email arrives within a minute.
Click the link to activate the account. Until you do, you can sign
in but most write actions are gated.

## 2. Create a repository

Sign in and click "New repository" (or visit `/new`).

- **Name** — alphanumerics and dashes; can't collide with your
  existing repos.
- **Visibility** — public (anyone can read) or private (only you
  + collaborators).
- **Initialize** — optionally add a README, a `.gitignore` (pick
  from the templates), and a license. If you initialize, the repo
  has commits immediately and you can clone right away. If you
  don't, the repo is empty and you'll see push instructions.

## 3. Clone the repo

The repo page shows a "Code" dropdown with a clone URL. For
HTTPS you'll need a [personal access
token](./personal-access-tokens.md) — your account password does
not work for git operations.

```sh
git clone https://shithub.example/<your-username>/<repo>.git
cd <repo>
```

When prompted for credentials, the username is your shithub
username and the password is the PAT.

## 4. Push a commit

```sh
echo "hello" >> README.md
git add README.md
git commit -m "say hello"
git push origin main
```

Refresh the repo page — your commit appears in the history. If
this is your first push, the repo's default branch is set to
whatever you pushed (usually `main`).

## Where to go next

- [Set up SSH](./ssh.md) so you don't have to enter a token every
  push.
- [Open your first issue](./issues.md).
- [Open a pull request](./pull-requests.md) — branch, push,
  compare.
- [Configure notifications](./notifications.md) so you don't get
  buried.
