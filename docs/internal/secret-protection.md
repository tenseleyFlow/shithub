# Secret Protection

SP26 turns the existing personal secret scanning substrate into the first
organization Secret Protection baseline. The owning code lives in
`internal/secretscan`, `internal/worker/jobs/secret_scan_history.go`,
`cmd/shithubd/hook_secret_protection.go`,
`internal/web/handlers/repo/security_secret_scanning.go`,
`internal/web/handlers/api/secret_scanning.go`, and
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
  `secret_bypass_controls`. Custom patterns are implemented by SP26a;
  bounded push-protection bypass controls are implemented by SP26b.
- Enterprise remains contact-sales only.

## Pattern Engine

`internal/secretscan` currently ships a curated, low-false-positive set:
AWS access key IDs, GitHub token formats, GitLab PATs, Stripe live/test
secret keys, Slack token prefixes, and private-key headers.

SP26d adds a local provider capability registry for those built-in
patterns. The registry records GitHub reference support for provider
notification and validity checks separately from shithub instance
support. SP26d closeout ratifies provider notification and provider-side
validity checks as unsupported for paid-organization GA: shithub has no
provider validators, no provider notifiers, and no outbound provider egress
for these surfaces. Repository UI, plan comparison, and API responses report
validity and provider notification as `unsupported` rather than claiming a
provider was contacted.

The scanner returns pattern name, line number, and a redacted excerpt.
Raw matched secret bytes must not be stored, logged, or printed. New
patterns are append-only unless a migration updates stored allowlist
rows keyed by pattern name.

Team organization owners can define organization-scoped custom patterns
from `/organizations/{org}/settings/security/secret-patterns`. Custom
pattern rows store only detector metadata: name, description,
RE2-compatible regular expression, minimum match length, enabled state,
and actor timestamps. The persisted raw expression is configuration, not
a secret; matched bytes are still redacted by the scanner. Findings use
stable `custom/<name>` labels so repository allowlists can suppress a
custom false positive by `(pattern, path)`.

Custom pattern writes validate the name shape, expression length,
regular-expression syntax, empty-string matches, and minimum match
length before persistence. Enabled patterns are loaded for org-owned
repository scans only when the org has the Team
`secret_custom_patterns` entitlement. Free orgs do not run or list
stored custom patterns, including rows left behind after a downgrade.

## Historical Scanning

`repo:secret_scan_history` scans repository history asynchronously. It
skips files larger than the worker cap, applies the repository
allowlist, and persists findings in `secret_scan_findings` without raw
secret values. `secret_scan_allowlist` stores `(repo_id, pattern, path)`
rows; adding an allowlist entry also sweeps matching open findings to
`allowlisted`.

The repository security page lists findings, allowlist entries, and
push-protection bypass requests. Free private org repositories see an
upgrade affordance rather than private finding details or exact bypass
request paths. The organization security overview loads secret
summary/finding queries only after the Team `secret_scanning`
entitlement passes. Custom pattern rows are not loaded unless
`secret_custom_patterns` is allowed.

SP26c adds read-only API routes for repository secret scanning status,
alerts, allowlist rows, and bypass requests:

- `GET /api/v1/repos/{owner}/{repo}/secret-scanning/status`
- `GET /api/v1/repos/{owner}/{repo}/secret-scanning/alerts`
- `GET /api/v1/repos/{owner}/{repo}/secret-scanning/allowlist`
- `GET /api/v1/repos/{owner}/{repo}/secret-scanning/bypass-requests`

The API uses `repo:read` PAT scope plus the same repository settings
policy action as the browser page. Private organization repositories
require Team before alert, allowlist, or status metadata is loaded, and
bypass requests additionally require `secret_bypass_controls`. API
responses omit raw secret bytes and omit the redacted excerpt column;
clients receive pattern names, normalized secret type labels, provider
capability metadata, validity/provider-notification status, paths, line
numbers, commit OIDs, states, actors, timestamps, and counts only.

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
tuple. When push protection blocks a finding, the hook records a
bounded pending bypass request keyed to `(repo, pattern, path, commit,
line)` and prints the repository security-page review URL. Approved,
unexpired bypasses suppress only that exact tuple. Denied and expired
bypasses do not suppress future rejections; expired approvals are
refreshed back to pending on the next matching push. Bypass rows store
only metadata and never raw match bytes or redacted excerpts.

Public repositories and personal private repositories can review
bypasses as a baseline owner-control surface. Private organization
repositories require the Team `secret_bypass_controls` entitlement
before exact bypass rows or approval controls are loaded. Approve/deny
events write structured audit rows (`secret_bypass_requested`,
`secret_bypass_approved`, `secret_bypass_denied`) with request id,
pattern, path, line, commit oid, and expiry only.

Team org custom patterns participate in the same push-time scan path
when `secret_custom_patterns` is allowed; invalid legacy rows are skipped
so broken configuration cannot deny all pushes.

## Known Gaps

- The scan history API reports the current finding store, not a durable
  worker-job history ledger.
- Provider notification and provider-side validity checks have capability
  metadata and truthful unsupported API/UI states. They are not implemented,
  are not advertised as Team features, and require a future provider-egress
  sprint before the status can become `unknown`, `active`, `inactive`, `sent`,
  or `failed`.
- Non-prefixed high-entropy generic heuristics remain deferred until
  the false-positive UX is stronger.

Do not describe any of those gaps as shipped in public pricing copy or
settings UI.
