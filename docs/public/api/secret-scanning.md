<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Secret scanning API

Secret scanning endpoints expose repository Secret Protection metadata to
automation clients. They require a PAT with `repo:read` scope and the
underlying user must be allowed to manage repository settings. Responses never
include raw secret values, and the alerts endpoint intentionally omits the
stored redacted excerpt.

Private organization repositories require the Team `secret_scanning`
entitlement for status, alert, and allowlist metadata. Private organization
bypass request metadata additionally requires `secret_bypass_controls`.
Unauthorized or unentitled private organization requests return `402` without
paths, pattern rows, or finding locations.

Provider notification and provider-side validity checks are reported
truthfully. Until an operator-configured provider integration ships, alert
responses mark those surfaces as `unsupported`; no API response implies that
shithub has contacted a third-party provider.

## Routes

GET /api/v1/repos/{owner}/{repo}/secret-scanning/status
GET /api/v1/repos/{owner}/{repo}/secret-scanning/alerts
GET /api/v1/repos/{owner}/{repo}/secret-scanning/allowlist
GET /api/v1/repos/{owner}/{repo}/secret-scanning/bypass-requests

## Status

`GET /api/v1/repos/{owner}/{repo}/secret-scanning/status` returns aggregate
repository scan metadata derived from current findings, allowlist rows, and
bypass requests:

```json
{
  "enabled": true,
  "visibility": "private",
  "feature_key": "secret_scanning",
  "total_alert_count": 3,
  "open_alert_count": 2,
  "resolved_alert_count": 0,
  "allowlisted_alert_count": 1,
  "stale_alert_count": 0,
  "allowlist_count": 1,
  "bypass_controls_available": true,
  "bypass_controls_feature_key": "secret_bypass_controls",
  "bypass_request_count": 1,
  "latest_finding_observed_at": "2026-05-20T12:30:00Z",
  "scan_history_backing": "findings",
  "raw_secret_material_included": false,
  "validity_checks_available": false,
  "provider_notifications_available": false
}
```

`scan_history_backing: "findings"` means this endpoint reports the current
finding store. It is not a durable worker-job history ledger.

## Alerts

`GET /api/v1/repos/{owner}/{repo}/secret-scanning/alerts` returns a paginated
array. `status` may be `open`, `resolved`, `allowlisted`, or `stale`.

```json
[
  {
    "id": 42,
    "number": 42,
    "state": "open",
    "status": "open",
    "secret_type": "github_token",
    "secret_type_display_name": "GitHub token",
    "provider_slug": "github",
    "pattern_category": "provider",
    "validity": "unsupported",
    "validity_check": {
      "supported_by_github": true,
      "supported_by_instance": false,
      "status": "unsupported",
      "description": "GitHub supports validity checks for this provider pattern, but this shithub instance has no configured provider validator."
    },
    "provider_notification": "unsupported",
    "provider_notification_capability": {
      "supported_by_github": true,
      "supported_by_instance": false,
      "status": "unsupported",
      "description": "GitHub supports partner/provider notification for this pattern, but this shithub instance has no configured provider notifier."
    },
    "path": "config/secrets.env",
    "line": 7,
    "commit_sha": "8c4e3f2a1b...",
    "first_seen_sha": "8c4e3f2a1b...",
    "created_at": "2026-05-20T12:30:00Z",
    "updated_at": "2026-05-20T12:30:00Z",
    "html_url": "https://shithub.example/owner/repo/security/secret-scanning"
  }
]
```

The response does not include `secret`, `excerpt`, or redacted excerpt fields.
`validity` may be `unsupported`, `unknown`, `active`, or `inactive`.
`provider_notification` may be `unsupported`, `disabled`, `pending`, `sent`,
or `failed`. The current built-in provider registry reports `unsupported` for
provider egress until a real integration is configured.

## Allowlist

`GET /api/v1/repos/{owner}/{repo}/secret-scanning/allowlist` returns
repository false-positive allowlist rows:

```json
{
  "total_count": 1,
  "allowlist": [
    {
      "id": 7,
      "pattern": "Stripe key",
      "path": "fixtures/test.env",
      "reason": "known test credential",
      "created_by": {"id": 1, "login": "alice", "type": "User"},
      "created_at": "2026-05-20T12:30:00Z"
    }
  ]
}
```

## Bypass Requests

`GET /api/v1/repos/{owner}/{repo}/secret-scanning/bypass-requests` returns
bounded push-protection bypass requests. Bypasses are exact
`(pattern, path, commit, line)` metadata and do not include matched secret
bytes.

```json
{
  "total_count": 1,
  "bypass_requests": [
    {
      "id": 9,
      "pattern": "GitHub token",
      "path": "config/secrets.env",
      "commit_sha": "8c4e3f2a1b...",
      "line": 7,
      "status": "pending",
      "requested_by": {"id": 1, "login": "alice", "type": "User"},
      "request_reason": "false positive",
      "created_at": "2026-05-20T12:30:00Z",
      "updated_at": "2026-05-20T12:30:00Z",
      "last_seen_at": "2026-05-20T12:30:00Z"
    }
  ]
}
```
