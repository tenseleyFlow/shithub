# Cloning over SSH

SSH lets you push and pull without re-entering credentials each
time. shithub authenticates each connection by SSH public key:
the key fingerprint maps to a user, and the session is locked to
the git protocol — you cannot get a shell.

## 1. Generate an SSH key

Skip this if you already have a key you're happy with
(`~/.ssh/id_ed25519.pub` typically).

```sh
ssh-keygen -t ed25519 -C "you@example.com"
```

Accept the default path. Set a passphrase if you want belt-and-
braces; `ssh-agent` will remember it for the session.

## 2. Copy the public key

```sh
cat ~/.ssh/id_ed25519.pub
```

Copy the entire line, including the `ssh-ed25519` prefix and the
trailing comment.

## 3. Add the key in shithub

Settings → SSH and GPG keys → "New SSH key". Paste the key, give
it a label (e.g., "laptop"), save.

The page shows the fingerprint shithub computed; verify it matches
what `ssh-keygen -l -f ~/.ssh/id_ed25519.pub` prints locally.

## 4. Test the connection

```sh
ssh -T git@shithub.sh
```

You'll see a confirmation message. The `-T` disables PTY allocation;
shithub's SSH service refuses TTYs anyway.

## 5. Clone with SSH

```sh
git clone git@shithub.sh:<owner>/<repo>.git
```

Subsequent pushes don't prompt — the agent presents the key, the
server matches the fingerprint to your account, and the session
is locked to `git-receive-pack` / `git-upload-pack`.

## Multiple shithub accounts on one machine

The repo path does not choose the account. For SSH URLs, the account is
the user that owns the key your SSH client offers. If you use more than
one shithub account from the same laptop, give each account its own host
alias:

```sshconfig
Host shithub-work
  HostName shithub.sh
  User git
  IdentityFile ~/.ssh/id_ed25519_work
  IdentitiesOnly yes

Host shithub-personal
  HostName shithub.sh
  User git
  IdentityFile ~/.ssh/id_ed25519_personal
  IdentitiesOnly yes
```

Then use the matching alias in each repo remote:

```sh
git remote set-url origin git@shithub-work:work-org/private-repo.git
git remote set-url origin git@shithub-personal:you/private-repo.git
```

Per-repo `core.sshCommand` works too:

```sh
git config core.sshCommand \
  "ssh -i ~/.ssh/id_ed25519_work -o IdentitiesOnly=yes"
```

If the wrong key is offered for a private repo, shithub returns
`repository not found`. That is intentional: it avoids confirming that
someone else's private repo exists.

## Removing or rotating a key

Settings → SSH and GPG keys lists every key on the account with
its last-used timestamp. Remove a key the moment a device is lost
or decommissioned — the next `git push` from that device will be
rejected.
