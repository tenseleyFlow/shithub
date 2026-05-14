# Rulesets

Read-only rulesets surface. Mirrors GitHub's
[`/repos/{owner}/{repo}/rulesets`](https://docs.github.com/en/rest/repos/rules)
response shape. shithub synthesizes ruleset rows from the
`branch_protection_rules` table — one branch-protection row
becomes one ruleset, named after its pattern, with each
configured field projected as a typed rule object.

All endpoints require `Authorization: Bearer <pat>` with
`repo:read` and gate on `ActionRepoRead`. The
[common API conventions](overview.md) apply.

## Ruleset shape

```json
{
  "id": 17,
  "name": "Pattern: main",
  "target": "branch",
  "source_type": "Repository",
  "source": "alice/demo",
  "enforcement": "active",
  "conditions": {
    "ref_name": {
      "include": ["refs/heads/main"],
      "exclude": []
    }
  },
  "rules": [
    {
      "type": "pull_request",
      "parameters": {
        "required_approving_review_count": 1,
        "dismiss_stale_reviews_on_push": false,
        "require_code_owner_review": true
      }
    },
    {"type": "non_fast_forward"},
    {"type": "deletion"},
    {"type": "required_signatures"},
    {
      "type": "required_status_checks",
      "parameters": {
        "required_status_checks": [{"context": "ci/build"}],
        "strict_required_status_checks_policy": false
      }
    }
  ],
  "created_at": "2026-05-12T04:00:00Z",
  "updated_at": "2026-05-13T04:00:00Z"
}
```

### Rule types

Each `rules[].type` maps to one or more columns on the underlying
protection row:

| `type` | Columns | Notes |
|--------|---------|-------|
| `pull_request` | `require_pr_for_push`, `required_review_count`, `require_code_owner_review`, `dismiss_stale_reviews_on_push` | Emitted when any of these is non-default. `parameters.required_approving_review_count` is the headline value. |
| `non_fast_forward` | `prevent_force_push` | Force-push gate. No parameters. |
| `deletion` | `prevent_deletion` | Branch-delete gate. No parameters. |
| `required_signatures` | `require_signed_commits` | Verified-signature gate. No parameters. |
| `required_status_checks` | `status_checks_required`, `dismiss_stale_status_checks_on_push` | `parameters.required_status_checks` is an array of `{context}` objects. `integration_id` is omitted (we don't track per-app integrations). |

Rules an admin hasn't configured are absent from the array — clients should treat missing as "rule not configured" rather than assuming defaults.

### Field notes

- `target` — always `"branch"`. Tag rulesets ship in a future sprint when tag protection lands.
- `source_type` / `source` — always `"Repository"` / `"<owner>/<repo>"`. Org-scoped and enterprise-scoped rulesets are out of scope until shithub grows enterprise-tier accounts.
- `enforcement` — always `"active"`. We don't model `"disabled"` or `"evaluate"` modes; every row in the table is enforced by the pre-receive hook.
- `conditions.ref_name.include` — single-entry array with the pattern prefixed by `refs/heads/`. shithub's patterns use `filepath.Match` semantics (`*` doesn't cross `/`), not gh's fnmatch.
- `bypass_actors` — not emitted. Bypass / multi-actor exemptions ship in a future sprint.

## List rulesets

```
GET /api/v1/repos/{owner}/{repo}/rulesets
```

Required scope: `repo:read`.

Returns every ruleset on the repo, ordered by id. Empty repos and repos with no configured protection rules return `[]`.

## Get a single ruleset

```
GET /api/v1/repos/{owner}/{repo}/rulesets/{id}
```

Required scope: `repo:read`.

Returns the single ruleset whose id matches. 404 when the id doesn't belong to this repo — same status as "doesn't exist" so the response doesn't leak existence across repo boundaries.

## Rules applying to a branch

```
GET /api/v1/repos/{owner}/{repo}/rules/branches/{branch}
```

Implementation route literal: `GET /api/v1/repos/{owner}/{repo}/rules/branches/*`.

Required scope: `repo:read`.

Returns every ruleset whose pattern matches the given branch name. **All** matching patterns are returned, not just the longest-match — the longest-match heuristic shithub's pre-receive enforcer uses is an internal precedence detail, not a contract surface.

Branch names that contain `/` (`feature/x`, `release/v1.0`) are matched against patterns using `filepath.Match`: `*` does not cross path separators, so `release/*` matches `release/v1.0` but not `release/v1.0/sub`.

## Errors

| Status | When                                            |
|-------:|-------------------------------------------------|
|    401 | PAT missing/invalid/expired/revoked.            |
|    403 | PAT lacks `repo:read` scope.                    |
|    404 | Repo not found, ruleset id not found, or cross-repo lookup. |

## Mutating rulesets

Creating, updating, and deleting rulesets via REST is not yet shipped. Use the web UI at `Settings → Branches → Add rule` for now. The shape of the future POST/PATCH/DELETE endpoints will mirror gh's documented surface and will require `repo:write` + `ActionRepoAdmin`.
