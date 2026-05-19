# Secret Protection

SP26 turns the existing personal secret scanning substrate into the first
organization Secret Protection baseline. The owning code lives in
`internal/secretscan`, `internal/worker/jobs/secret_scan_history.go`,
`cmd/shithubd/hook_secret_protection.go`,
`internal/web/handlers/repo/security_secret_scanning.go`, and
`internal/web/handlers/orgs/security.go`.

## Product Contract

- Public repositories get supported-pattern secret scanning and push
  protection as a baseline.
- Private personal repositories get push-time blocking as a baseline;
  historical private-repo scan history remains a personal Pro feature.
- Private organization repositories require Team for historical scan
  views, on-demand scans, organization security overview aggregation,
  and pre-receive push protection.
- The Team feature keys are `secret_scanning`,
  `secret_push_protection`, `secret_custom_patterns`, and
  `secret_bypass_controls`. The latter two are registry keys only until
  their implementation sprints ship.
- Enterprise remains contact-sales only.

## Pattern Engine

`internal/secretscan` currently ships a curated, low-false-positive set:
AWS access key IDs, GitHub token formats, GitLab PATs, Stripe live/test
secret keys, Slack token prefixes, and private-key headers.

The scanner returns pattern name, line number, and a redacted excerpt.
Raw matched secret bytes must not be stored, logged, or printed. New
patterns are append-only unless a migration updates stored allowlist
rows keyed by pattern name.

## Historical Scanning

`repo:secret_scan_history` scans repository history asynchronously. It
skips files larger than the worker cap, applies the repository
allowlist, and persists findings in `secret_scan_findings` without raw
secret values. `secret_scan_allowlist` stores `(repo_id, pattern, path)`
rows; adding an allowlist entry also sweeps matching open findings to
`allowlisted`.

The repository security page lists findings and allowlist entries. Free
private org repositories see an upgrade affordance rather than private
finding details. The organization security overview loads secret
summary/finding queries only after the Team `secret_scanning`
entitlement passes.

## Push Protection

`pre-receive` runs secret push protection before branch protection for
pushes that add reachable objects. It scans newly reachable commits
using git plumbing:

1. `git rev-list --reverse <new> --not --all` finds new commits.
2. `git diff-tree --root --no-commit-id -r -m --name-only -z
   --diff-filter=ACMRT <commit>` finds changed paths.
3. Blob contents are read with `repogit.ReadBlobBytes`.
4. The same `secretscan.Scan` pattern engine runs with a 256 KiB
   per-blob cap.

The hook skips `.git*`, `vendor/`, `node_modules/`, `dist/`, oversized
blobs, and likely-binary blobs. It stops after 10 findings to keep the
rejection bounded. The friendly hook error prints only path, line,
pattern, and short commit SHA; it never includes the redacted excerpt or
raw secret bytes.

Allowlist rows suppress push rejection for the exact `(pattern, path)`
tuple. There is no bypass request queue yet, so a false positive must be
allowlisted from the repository security page before pushing again.

## Known Gaps

- Custom pattern CRUD and enforcement are not implemented yet.
- Push-protection bypass requests, approvals, and audit events are not
  implemented yet.
- Scan history API endpoints are not implemented yet.
- Provider notification and provider-side validity checks are not
  implemented yet.
- Non-prefixed high-entropy generic heuristics remain deferred until
  the false-positive UX is stronger.

Do not describe any of those gaps as shipped in public pricing copy or
settings UI.
