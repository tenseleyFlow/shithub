# SSH git protocol

Wires `git@shithub.sh:owner/repo.git` clone/push to the same
authorize-and-dispatch path as the HTTPS git surface. Three pieces
must all be in place; missing any gives users a clone URL that
errors at connect:

1. **Code** (`internal/auth/policy/...`, `cmd/shithubd/ssh.go`):
   `shithubd ssh-authkeys <fingerprint>` returns the matching
   user's authorized_keys line with a forced `command=` that runs
   `shithubd ssh-shell <user_id>`. The shell subcommand parses
   `SSH_ORIGINAL_COMMAND`, authorizes, and `syscall.Exec`s into
   `git-{upload,receive}-pack`. Already in tree (S13).
2. **sshd** (`deploy/sshd_config.j2`): a `Match User git` block
   wires `AuthorizedKeysCommand /usr/local/bin/shithubd
   ssh-authkeys %f` and `AuthorizedKeysCommandUser shithub-ssh`.
3. **Config** (`web.env`): `SHITHUB_AUTH__SSH__ENABLED=true` and
   `SHITHUB_AUTH__SSH__HOST=git@<domain>`. Without this, the
   sshd path is reachable but the UI hides it from clone widgets.

## First-time enable on an existing droplet

If S13 shipped before the deploy ever ran SSH-git:

```sh
# 1. Create the git login user with git-shell as a defense layer
ssh root@shithub.sh '
  apt-get install -y git  # provides git-shell
  useradd --system --home-dir /var/lib/git --create-home \
          --shell "$(which git-shell)" git
  getent passwd git
'

# 2. Confirm sshd_config has the Match User git block
ssh root@shithub.sh '
  grep -A4 "Match User git" /etc/ssh/sshd_config
'
# Expected: AuthorizedKeysCommand /usr/local/bin/shithubd ssh-authkeys %f
#           AuthorizedKeysCommandUser shithub-ssh
#           PermitTTY no
#           AllowAgentForwarding no

# 3. Set the env knobs on web.env (worker.env doesn't render repo pages)
ssh root@shithub.sh '
  printf "\nSHITHUB_AUTH__SSH__ENABLED=true\nSHITHUB_AUTH__SSH__HOST=git@shithub.sh\n" \
    >> /etc/shithub/web.env
'

# 4. Restart shithubd-web to pick up the new env (workers don't need it)
ssh root@shithub.sh 'systemctl restart shithubd-web'
```

Verify:

```sh
# (a) repo page now shows the SSH clone URL
curl -fsS https://shithub.sh/tenseleyflow/shithub | grep -i 'ssh' | head

# (b) the git user exists with git-shell
ssh root@shithub.sh 'getent passwd git'
# git:x:117:65534::/var/lib/git:/usr/bin/git-shell

# (c) AKC handshake works for an unknown fingerprint (returns nothing,
#     exits 0 — that's the fail-closed contract):
ssh root@shithub.sh '/usr/local/bin/shithubd ssh-authkeys SHA256:not-a-real-key 2>&1; echo rc=$?'
# (empty output, rc=0)

# (d) end-to-end clone with a key you've added at /settings/keys:
ssh-keygen -t ed25519 -f ~/.ssh/shithub-test -N '' -C 'shithub-ssh-test'
cat ~/.ssh/shithub-test.pub
# paste into https://shithub.sh/settings/keys
GIT_SSH_COMMAND='ssh -i ~/.ssh/shithub-test -o IdentitiesOnly=yes' \
  git clone git@shithub.sh:tenseleyflow/shithub.git /tmp/shithub-ssh-clone
```

If (d) hangs or rejects:
- `journalctl -u ssh -n 50 --no-pager | grep -E "git|AKC|authkeys"`
- `journalctl -u shithubd-web -n 100 | grep ssh-shell`
- Manually invoke the AKC with the real fingerprint:
  `ssh root@shithub.sh "/usr/local/bin/shithubd ssh-authkeys SHA256:<your-fp>"`
  Should print one line starting with `command="...shithubd ssh-shell..."`.

## When something breaks

| Symptom | Likely cause |
|---|---|
| `git clone git@shithub.sh:...` hangs at "Connecting" | sshd not listening on 22, or firewall blocking. `systemctl status ssh; ufw status` |
| Hangs at "Permission denied (publickey)" | Key not in user's `/settings/keys`, OR fingerprint mismatch. The DB stores `SHA256:...` from `ssh-keygen -lf <key>`. |
| "Permission denied" right after key offer | AKC returned an empty string (key DOES match no row). Run `shithubd ssh-authkeys <your-fp>` manually as root to debug. |
| Connects then "fatal: protocol error" | `ssh-shell` rejected the requested op. Check `journalctl -u shithubd-web` for the `ssh-shell: denied` entry. |
| `git push` works but post-receive hooks don't fire | `SHITHUB_*` env not threaded through. Check `protocol.PrepareDispatch` — that's the env-build site. |

## Disabling without removing

Set `SHITHUB_AUTH__SSH__ENABLED=false` in `web.env` and restart
shithubd-web. The clone widget hides the SSH URL but the sshd path
still works for anyone who knows it. Use this when the SSH service
is having issues but you don't want to alarm people who try the UI.

To fully disable: comment the `Match User git` block in
`/etc/ssh/sshd_config` and reload sshd. The `git` user can stay.
