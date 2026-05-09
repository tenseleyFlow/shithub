# Account settings

Your settings are split into a small number of pages reachable
from the avatar menu.

## Profile

Display name, bio, location, website, company, pronouns. Visible
on your profile page (`/<username>`). Markdown is **not** rendered
in the bio — it's plain text with hyperlinks auto-linked.

## Emails

You can have multiple email addresses on one account. One is
**primary** (used for notifications and as the default committer
match), the others are secondary (still match commits authored
with that email).

Each email is independently verified — until you click the link
sent to it, that address can't be used as primary.

## Password

Change with the current password. Successful change invalidates
**every** session except the current one (the session-epoch
mechanism). Other devices will be signed out.

## Two-factor authentication

See [TOTP & recovery codes](./2fa.md). Strongly recommended.

## Sessions

Lists every active session with device + last-used time. "Sign
out everywhere" bumps your session epoch — every other session is
killed instantly. Use this if you suspect an unauthorized sign-in.

## SSH and GPG keys

- **SSH keys** authenticate `git@shithub.sh:...` operations.
- **GPG keys** verify signed commits — when a commit's signature
  matches a registered GPG key, the commit shows a "Verified"
  badge in history.

Each key shows last-used timestamp; rotate when devices are lost.

## Personal access tokens

See [PATs](./personal-access-tokens.md).

## Notifications

See [Notifications](./notifications.md).

## Audit log

Settings → Audit log lists security-relevant actions on your
account: sign-ins, password changes, 2FA toggles, key add/remove,
PAT create/revoke, suspension events. Each row carries the IP and
user-agent at the time. Export as JSON if you need to share with
an operator during incident response.

## Delete account

The bottom of Settings → Account has a delete button. It requires
typing your username for confirmation. Deletion:

- Marks the account for soft-delete with a 30-day grace.
- Hides your repos, issues, comments, and PRs immediately.
- Frees the username after grace; until then, the username is
  reserved.

Within the grace period, signing in restores the account fully.
After grace, the deletion is final; comments and issues you
authored are reattributed to a `ghost` user.
