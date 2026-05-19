# Code security

SP27 adds the first code-scanning product surface for hosted
organizations. The implementation lives in `internal/repos/codescan`,
`internal/repos/queries/code_scanning.sql`, and the repo/org security
handlers under `internal/web/handlers`.

## Product contract

shithub supports external scanner ingestion through SARIF 2.x uploads.
It does not execute scanners itself in SP27 and does not claim parity
with any one scanner engine. Operators and repository automation can
run the scanner they trust, then POST a SARIF report to the repository
code scanning upload endpoint.

The upload path normalizes each supported result into durable alert
metadata:

- scanner tool name, optional tool GUID, and upload category;
- rule ID, rule name, message, severity, file path, and location;
- commit SHA and ref name supplied by the uploader;
- stable fingerprint used to deduplicate repeated uploads.

Raw SARIF is not retained. shithub stores a SHA-256 digest on the
upload row for audit/debug correlation and discards the original
payload after normalization.

## Billing gates

Public repositories can ingest and view code scanning alerts without
Team billing.

Private organization repositories require an active or in-grace Team
subscription for:

- `code_scanning`: SARIF uploads and private-org alert views;
- `security_campaigns`: grouping selected alerts into remediation
  campaigns and closing/reopening those campaigns.

The org security overview requires `security_overview` first, then
loads code scanning details only when `code_scanning` is allowed. A
locked organization receives upgrade copy and alert details are not
queried.

## Security behavior

Repo visibility and collaborator access are still the outer gates.
Repo code scanning pages use `policy.ActionRepoRead`; uploads require
`policy.ActionRepoWrite`; campaign creation and state changes require
`policy.ActionRepoSettingsGeneral`.

The SARIF parser is intentionally conservative:

- empty, oversized, invalid, or no-run payloads are rejected;
- unsupported result shapes are skipped instead of guessed;
- string fields are clamped to the database shape constraints;
- valid zero-alert SARIF uploads are accepted so clean scanner runs can
  be recorded.

Alerts are ordered by severity and `last_seen_at`. Re-uploading the
same finding refreshes `last_seen_at`, updates mutable metadata, and
keeps dismissed alerts dismissed.

## Campaigns

Campaigns are repository-scoped groups of code scanning alerts. A
maintainer selects stored alerts, creates a campaign with a title and
optional description, then can close or reopen the campaign. Campaign
state is stored separately from alert state; closing a campaign does
not dismiss the underlying alerts.

## Deferred work

- Scanner execution workers and managed scanner configuration.
- SARIF upload API tokens / CI-native endpoint shape.
- Alert dismissal UI and bulk state transitions.
- Cross-repository campaign creation from the organization overview.
- Rich SARIF thread flows, secondary locations, and result attachments.
