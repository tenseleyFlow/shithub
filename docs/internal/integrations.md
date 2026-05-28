# Integrations

shithub's first app-style integration surface is deliberately narrow:
signed webhooks plus external check runs. This gives organizations the
core automation loop without claiming full GitHub Apps parity.

## Organization Webhooks

Organization owners can manage webhooks at:

- `GET /organizations/{org}/settings/hooks`
- `GET /organizations/{org}/settings/hooks/new`
- `POST /organizations/{org}/settings/hooks`
- `GET /organizations/{org}/settings/hooks/{id}`

The handlers store rows in `webhooks` with `owner_kind = 'org'` and
`owner_id = org.id`. Repo domain events for repositories owned by that
organization fan out to both repo-level webhooks and matching org-level
webhooks. User-owned repositories do not fan out to organization
webhooks.

Webhook secrets are encrypted at rest with `internal/auth/secretbox`.
Create and update paths run the same SSRF validation as repo webhooks:
scheme/port checks, DNS resolution, and private/loopback address
rejection unless the operator explicitly configures an allow-list.

Every lifecycle action records an org-targeted audit row:

- `webhook_created`
- `webhook_updated`
- `webhook_deleted`
- `webhook_active_set`
- `webhook_active_unset`
- `webhook_pinged`
- `webhook_redelivered`

## External Check Runs

The Checks API remains the current integration point for CI systems.
External systems post check runs with `app_slug = 'external'`, and
branch protection evaluates named check-run contexts through the policy
and branch-protection gates. This is not a GitHub App installation
model; it is the baseline external status/check mechanism.

## Billing Placement

Organization webhooks and external check runs are baseline
functionality. Team plan value comes from using these integrations
against private organization repositories alongside protected branches,
required checks, Actions settings, security scanning, SBOMs, and
artifact attestations.

## Deferred GitHub Apps Parity

Still deferred:

- app registration with manifests and callback URLs;
- installation records per organization/repository;
- app-scoped authentication, JWT signing, and installation tokens;
- per-app permission grants and webhook event subscriptions;
- marketplace/listing/review flows.

Until those ship, product copy should say "app-style integrations" or
"organization webhooks and checks" rather than "GitHub Apps".
