# Issues

Issues track bugs, ideas, and conversations against a repository.
Anyone with read access to a repo can see its issues; opening and
commenting depends on the repo's settings (open to all logged-in
users by default).

## Opening an issue

Repo → Issues → "New issue". Required:

- **Title** — one line.
- **Body** — markdown ([reference](./markdown.md)). Drag-and-drop
  attachments upload to the repo's blob store.

Optional:

- **Labels** — colored tags maintainers use for triage.
- **Assignees** — who's expected to handle it.
- **Milestone** — group issues toward a release/target.

## References

shithub auto-links these in issue + comment bodies:

- `#123` — issue or PR in this repo.
- `owner/repo#123` — issue or PR in another repo.
- `@username` — user mention; they get a notification.
- `@org/team` — team mention; every member is notified.
- Commit SHAs (full or 7+ chars) — link to the commit page.

## Closing

Three ways an issue closes:

- Manually, via the "Close" button.
- Via a referenced commit/PR — `Fixes #123` or `Closes #123` in a
  merged PR's title or body auto-closes the issue.
- Via API.

Closed issues stay visible; you can reopen them.

## Comments + reactions

Each comment supports the same markdown as the body. Reactions
(👍 👎 😄 🎉 😕 ❤️ 🚀 👀) live on every issue/comment as a way to
signal agreement without adding "+1" noise.

## Locking

Maintainers can lock a conversation. Locked issues accept no new
comments or reactions; existing content stays.

## Filters and search

The Issues list supports filters:

- `is:open` / `is:closed`
- `author:<user>`
- `assignee:<user>`
- `label:"good first issue"`
- `milestone:"v1.0"`
- `mentions:<user>`
- `commenter:<user>`
- Sort: newest, oldest, most commented, recently updated.

Free-text after the filter narrows by title + body.
