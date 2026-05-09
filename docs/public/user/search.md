# Search

Top-bar search opens a quick palette: type and you'll see matching
repos, users, issues, and code snippets. Pressing enter takes you
to the full search results page with filters.

The full results page (`/search`) supports four scopes:

- **Code** — file content + path matches across repos you can see.
- **Repositories** — repo names, descriptions, topics.
- **Issues + PRs** — across repos you can see.
- **Users** — usernames, display names.

## Filters and operators

The search bar supports a small grammar inspired by GitHub's:

| Operator                  | Effect                                                |
|---------------------------|-------------------------------------------------------|
| `repo:owner/name`         | Restrict to one repo.                                 |
| `org:name`                | Restrict to repos owned by an org.                    |
| `user:name`               | Restrict to a user's repos.                           |
| `path:src/`               | Restrict by file path prefix.                         |
| `extension:go`            | Restrict by file extension.                           |
| `language:go`             | Restrict by detected language.                        |
| `author:name`             | (issues/PRs) author.                                  |
| `commenter:name`          | (issues/PRs) someone commented.                       |
| `is:open` / `is:closed`   | (issues/PRs) state.                                   |
| `is:pr` / `is:issue`      | Narrow to PR or issue.                                |
| `label:"good first issue"`| (issues/PRs) labelled.                                |
| `milestone:"v1.0"`        | (issues/PRs) milestoned.                              |
| `created:2026-01-01..2026-04-01` | Date range (ISO 8601).                         |
| `-foo`                    | Negate.                                               |

Quoted strings are exact; unquoted strings are tokenized.

## Visibility rules

Search only ever returns results you would be allowed to see:

- Public repos and their content/issues/PRs are searchable
  without sign-in.
- Private repos require you to have read access (collaborator,
  team member, owner).
- Search through someone else's PAT or session never reveals
  results to your own queries — each user's search is scoped to
  their own visibility.

## Indexing latency

- New repos are indexed within seconds of the first push.
- Subsequent pushes update the index within a few seconds.
- Issue + PR text is indexed inside the same transaction that
  created the row — searchable immediately.

If you push but the new content isn't appearing in search after
~30 seconds, the indexer is backed up; try the file directly via
the repo's tree view to confirm the data exists. Operators
monitor index lag; persistent staleness is an alert.
