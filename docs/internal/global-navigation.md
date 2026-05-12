# Global Navigation And Dashboard Lists

The authenticated global navigation mirrors GitHub's non-Copilot dashboard
shortcuts:

| Surface | Route | Notes |
| --- | --- | --- |
| All issues | `/issues/assigned` | Left-rail views for assigned, created, mentioned, and recent activity. |
| All pull requests | `/pulls` | Tabs for created, assigned, mentioned, and review requests. |
| All repositories | `/repos` | Left-rail views for contributions, owned repositories, forks, and admin access. |
| Notifications | `/notifications?filter=unread` | Uses the S29 notification inbox. |
| New issue | `/issues/new` | Repository chooser that links into the existing repo-scoped issue form. |

These pages are authenticated browser surfaces. They reuse the canonical
repository visibility predicate from `internal/auth/policy` for list queries.
Issue and pull-request relationship filters use existing relational state:

- `issue_assignees` for assigned items.
- `issues.author_user_id` for created items.
- `notifications` / `notification_threads` reason rows for mentions and review
  requests.
- `notification_threads.subscribed` plus author/assignee state for recent
  activity.

The dashboard query inputs intentionally display GitHub-style search strings
(`is:issue state:open assignee:@me ...`) while the first implementation parses
only enough to preserve state and free-text filtering. Rich GitHub search
operators for these dashboard lists should move into the search parser before
being advertised as complete syntax support.

The global create menu excludes Copilot/agent actions by project policy. Entries
without a backing shithub feature, such as import repository and projects, render
disabled until their owning sprints ship.
