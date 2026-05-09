# Notifications

shithub notifies you when something happens that needs your
attention: an issue is assigned, a review is requested, a comment
mentions you, a watched repo has new activity.

Two delivery channels:

- **In-app inbox** at `/notifications`.
- **Email** to your primary email.

Both are driven by the same routing rules.

## Watch levels

For each repo, your subscription level is one of:

- **Ignore** — never notify, even on direct mentions.
- **Participating only** (default for repos you've never touched)
  — notify only when something concerns you directly: you're
  assigned, mentioned, your comment was replied to, your PR has
  a review.
- **All activity** — every issue, PR, comment, push gets a
  notification.
- **Custom** — pick which event categories you want.

Set the level from the repo header's "Watch" dropdown.

## Auto-subscribe

You're auto-subscribed to:

- Issues + PRs you opened.
- Issues + PRs you commented on (until you unsubscribe via the
  "Unsubscribe" button on the thread).
- Issues + PRs you're assigned to or your review is requested on.

Auto-subscribe lives at the **thread** level, not the repo level —
unsubscribing from one issue doesn't change anything else.

## Reading the inbox

The `/notifications` page lists unread + pinned threads.
- **Mark as read** — collapse and remove from the unread list.
- **Mark as done** — archive; comes back if there's new activity.
- **Mute thread** — never notify on this thread again, even on
  new mentions.

Filters: by repo, by reason (mention, review-requested, assigned,
etc.), and by read/unread.

## Email behavior

- Each notification is one email; we don't bundle (yet).
- Subject: `[<repo>] <issue title> (#<n>)`.
- Reply-by-email is **not supported** — replies bounce. We don't
  want to be in the email-thread-state-management business.
- Each email has a one-click **HMAC-signed unsubscribe link**
  that moves the thread to muted without a sign-in step.

## Stop emails entirely

Settings → Notifications → "Email delivery: never". The in-app
inbox keeps working.

## How frequently we email

There's no digest mode (yet). Emails go out as the events happen.
If that's too much, switch the repo to "Participating only" or
mute specific threads.
