# Branch protection

S20 ships the branches index, tags index, compare view, and the
basic branch-protection rule engine enforced by the pre-receive
hook.

## Routes

| Route                                                   | Handler                       |
| ------------------------------------------------------- | ----------------------------- |
| `GET /{owner}/{repo}/branches?filter=active|stale|`       | `branchesList`                |
| `GET /{owner}/{repo}/tags`                              | `tagsList`                    |
| `GET /{owner}/{repo}/compare`                           | `compareView`                 |
| `GET /{owner}/{repo}/compare/{base}...{head}`           | `compareView`                 |
| `GET /{owner}/{repo}/settings/branches`                 | `settingsBranches` (auth-gated) |
| `POST /{owner}/{repo}/settings/branches`                | upsert rule                   |
| `POST /{owner}/{repo}/settings/branches/{id}/delete`    | delete rule                   |
| `POST /{owner}/{repo}/settings/default-branch`          | swap default                  |

The compare route uses chi's wildcard because chi can't represent the
literal `...` separator in a route param. The handler parses
`<base>...<head>` (or `<base>..<head>`) server-side. Cross-repo head
shape `fork:branch` is accepted but currently treated as local — the
full cross-repo flow lives with S22 (PRs) + S27 (forks).

## Branches list

For each branch we show:

- Name (linked to `/tree/<branch>`)
- Last-commit subject + age (from `repogit.HeadOf`)
- Ahead/behind vs the default branch (from `repogit.AheadBehind`,
  i.e. `git rev-list --left-right --count default...branch`)
- Default badge + Protected badge (any rule whose pattern matches)
- "Compare" button → `/compare/<default>...<branch>`

Sort: default first, then by recent activity. Filter: `active` (within
90 days), `stale` (>90 days), or all.

Counts are recomputed per-request today; the spec proposes caching
ahead/behind on push (S14's `push:process`). That's a perf-pass item
and lives with the rest of the cache work in **S36**.

## Tags list

Lightweight: name, OID, subject, author age. Resolution uses
`repogit.HeadOf(gitDir, "tags/<name>")` — works for both lightweight
and annotated tags. The "Releases" placeholder note signals that
first-class releases ship post-MVP.

## Compare view

Inputs: `base` and `head` (defaults to `repo.default_branch` when
empty). The bare `/compare` route renders GitHub's branch/tag picker
blank slate instead of redirecting to `default...default`. Renders:

- Base/head dropdowns listing local branches and tags with live
  filtering. Cross-repo `fork:branch` input is still normalized to a
  local ref until fork PRs ship.
- A mergeability/status line plus a "Create pull request" button when
  `head` has commits not on `base`.
- The commits-list (head-side only) via
  `repogit.CommitsBetween(base, head, 250)`.
- The three-dot diff via S19's renderer fed from
  `diffsource.FromMergeBase`.

Empty state ("nothing to compare") fires when `Ahead == 0`.

## Repository protection rules

Stored in `branch_protection_rules`:

| Column                     | Default | Notes                                        |
| -------------------------- | ------- | -------------------------------------------- |
| `pattern`                  | (set)   | `filepath.Match` glob                        |
| `target`                   | `branch` | `branch` or `tag`; existing rows default to branch |
| `prevent_force_push`       | `true`  | branch non-fast-forward gate; tag movement gate |
| `prevent_deletion`         | `true`  | enforced                                     |
| `require_pr_for_push`      | `false` | enforced for direct branch git pushes and web edits |
| `allowed_pusher_user_ids`  | `{}`    | enforced; empty = no restriction              |
| `require_signed_commits`   | `false` | placeholder — post-MVP; branch-only input    |
| `status_checks_required`   | `{}`    | branch-only; enforced through the S24 check-run gate |
| `required_review_count`    | `0`     | branch-only; enforced through the PR review gate |

### Pattern matching

`filepath.Match` semantics:

- `*` matches any non-separator chars (does NOT cross `/`)
- `?` matches a single non-separator char
- `[abc]` matches one of a/b/c

`release/*` matches `release/v1.0` but NOT `release/v1.0/sub`. The
matcher is unit-tested in `protection_test.go::TestMatchRule_…`.

When multiple rules match a branch or tag within the same target, the
**longest pattern wins** (alphabetical tiebreaker). Branch and tag
rules are isolated even when their pattern text is identical.

### Enforcement (pre-receive)

The pre-receive hook (`cmd/shithubd/hook.go::hookPreReceiveCmd`):

1. Reads `<old> <new> <ref>` lines from stdin.
2. Runs the existing S15 policy gate (suspended/archived/deleted).
3. For each ref update, calls `protection.Enforce`:
   - Skip refs outside `refs/heads/*` and `refs/tags/*`.
   - Classify the ref as `branch` or `tag`.
   - Resolve the longest-matching rule.
   - For branches, apply gates in order: deletion → require-PR →
     force-push → allowed-pushers.
   - For tags, apply gates in order: deletion → tag movement →
     allowed-pushers.
   - Return a `Decision` with `Allow`, `Reason`, and `Pattern`.
4. On any `Allow=false`: write `protection.FriendlyMessage(d)` to
   stderr; exit non-zero.

**Force-push detection** uses `git merge-base --is-ancestor old new`
(via `repogit.IsAncestor`). When the old SHA is not an ancestor of
the new SHA, the update is non-fast-forward → reject.

**Tag movement** treats changing an existing tag (`old` and `new`
both non-zero) as the tag equivalent of a force-push. Git tag refs do
not have a useful commit ancestry relationship in all cases
(annotated tags point to tag objects), so tag rules block movement
directly when `prevent_force_push` is enabled.

**Require pull request** rejects direct branch creates and updates when
`require_pr_for_push` is true on the matching rule. That covers both
normal git pushes and in-browser file-editor commits because both paths
call `protection.Enforce` before advancing `refs/heads/*`. Branch
deletions remain controlled by `prevent_deletion`.

**Failure mode** (DB unavailable from hook): the rule lookup returns
an error; the hook prints "transient; retry" to stderr and exits
non-zero. **Fail closed**, per the S20 lean: better to reject a
legitimate push than to allow a force-push past a missing rule.

## Paid governance gates

SP18 makes the private organization repository governance surface a
Team entitlement while keeping public repositories generous. The web
settings handler first authorizes `repo:settings:branches` through
`internal/auth/policy`, then asks `internal/entitlements` whether the
billable repo owner can use:

- `advanced_branch_protection` for force-push/deletion/signature
  toggles, tag protection, allowed-pusher restrictions, and required
  status checks.
- `required_reviewers` for required approvals and multiple required
  reviewers.

Free private org repositories can read and remove existing rules, but
cannot add or expand gated settings. Team orgs with active or trialing
billing can use the controls. Past-due/canceled Team orgs keep rule
data and receive billing-action messaging until billing is restored.

Personal private repositories use the same feature keys with user-kind
entitlements; the Pro enforcement flags live in operator config and
are documented in `docs/internal/billing.md`.

## Default-branch change

`POST /settings/default-branch` validates the target exists, updates
`repos.default_branch`, then runs `git symbolic-ref HEAD refs/heads/<new>`
in the bare repo so new clones pick up the new default. The DB row is
the source of truth; symbolic-ref failure is logged but not rolled
back (operator can re-run).

Existing clones don't follow the change — that's git's nature, not a
bug. The settings UI will warn on this when a fuller settings nav
lands in S32.

## Audit log

Every protection-rule create/update/delete and default-branch change
emits an audit row through the existing recorder (`audit.TargetRepo`,
`ActionRepoCreated` placeholder slot — until S20-specific actions
land, the meta blob carries `action: "default_branch_changed"`/
`"create"`/`"update"`/`"delete"` discriminators).

## Tests

- `internal/repos/protection/protection_test.go` — pattern-match
  precedence (longest-prefix, alphabetical tiebreak, target
  isolation, no-cross-slash), ref classification, zero-SHA detection.
- `internal/repos/git/branchops_test.go` — covered by the existing
  log/blame test fixtures (initial commit suffices for AheadBehind +
  IsAncestor smoke).

## Pitfalls handled

- **Tag-only semantics**: tag rules deliberately ignore branch-only
  PR review, required-status-check, and signed-commit knobs. The
  settings handler strips those values on tag-targeted writes.
- **Pattern globs vs regexes**: deliberate; matches GitHub UX.
- **Pre-receive cache scope**: one DB read per hook invocation. With
  small rule sets this is cheap. A long-lived cache would complicate
  invalidation; defer.
- **Default-branch change while open clones**: existing clones have a
  stale HEAD; new clones pick up the new default. Standard git
  behavior; documented in the settings UI.
- **DB unreachable from hook**: fail-closed reject with retry message.

## Deferred to later sprints

- **`require_signed_commits` enforcement** → post-MVP signing surface.
- **Per-team allowed-pushers** → S31 (orgs/teams).
- **Ahead/behind caching on push** → S36.
- **Cross-repo compare full UI** → S22 + S27.
