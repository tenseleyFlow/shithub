# Shared Actions Runner Readiness

This document is the S41j-6 readiness packet for letting normal repositories
use the shithub.sh shared Actions pool. S41j is the safety and operations track.
S41k is the Actions UI parity track; S41k improves the product surface but is
not a blocker for controlled arbitrary-repo execution.

## Current Decision

Status: **controlled dogfood, not broad public GA**.

The platform can run ordinary repositories that meet the Actions policy, runner
label, and syntax constraints. Broad public shared-runner enablement should wait
until this checklist has a deployed/manual pass and no open Critical or High
findings.

## Eligibility Contract

A repository may use the shared pool when all of these are true:

1. Site Actions policy allows runner dispatch. The site switch is a hard kill
   switch and overrides repo/org policy.
2. The repo or its owner org has Actions enabled, or inherits an enabled site
   policy.
3. The workflow lives under `.shithub/workflows/*.yml` and parses under the
   supported v1 subset.
4. The triggering actor can run Actions on that repo. Untrusted pull requests
   queue in an approval-required state before runner dispatch.
5. The job's `runs-on` labels match an online runner, normally
   `ubuntu-latest` for the first shared Linux pool.
6. Repo queued-run, repo concurrency, owner concurrency, and actor hourly caps
   permit the run.
7. The repo is not archived or deleted.

For public shithub.sh rollout, operators should keep site caps conservative and
raise them only after real queue/claim/host-cost data exists.

## Billing And Entitlements

Billing is present, and PAYMENTS SP08 adds monthly Actions minute metering.
The current entitlement boundary includes:

- `org.actions_minutes_quota`
- `LimitOrgActionsMinutesQuota`

`LimitOrgActionsMinutesQuota` reports concrete monthly quotas for Free and
Team organizations. Do not gate public shared-runner execution by scattered
plan checks. Hard-deny billing gates must go through `internal/entitlements`
and keep authorization separate from entitlement denials.

Recommended rollout posture:

- personal/public dogfood repos: allowed only under site policy and conservative
  caps;
- organization-level Actions secrets/variables: already Team-gated;
- paid shared-runner minutes: keep in controlled rollout until metering can
  record usage and enforcement rejects over-quota dispatch consistently;
- unpaid or past-due orgs: keep paid-only Actions configuration read-only, but
  do not delete secrets, variables, or prior run history.

## S41j-6 Findings

| ID | Severity | Status | Finding | Resolution |
| --- | --- | --- | --- | --- |
| S41J6-H1 | High | Fixed in S41j-6 | Site Actions disable was not a hard kill switch; explicit repo/org enablement could still evaluate true and queued jobs could be claimed. | Effective policy and runner claim SQL now return false whenever `actions_site_policy.actions_enabled=false`. Tests cover enqueue-time policy and claim-time dispatch. |
| S41J6-M1 | Medium | In progress with compensating control | Actions minutes billing has usage accounting and numeric limits, but hard-deny dispatch enforcement is still landing in SP08. | Do not market broad paid shared-runner minutes yet. Use site/org/repo policy caps and runner capacity as the public-runner control until SP08 enforcement is complete. |
| S41J6-M2 | Medium | Manual validation pending | The S41j-5 arbitrary-repo smoke must run on production after deploy. | Run the scratch plus second-repo checklist in `runbooks/actions-runner.md` before declaring broad availability. |

No Critical findings are open in this packet.

## Required Evidence Before Broad Enablement

- `scripts/audit-actions-public-runners.sh` passes on the deployed commit.
- Focused Go tests pass for site kill switch, repo/owner concurrency caps,
  unsupported label diagnostics, token gates, and untrusted PR secret behavior.
- Live smoke passes on `mfwolffe/scratch`.
- Live smoke passes on at least one additional normal public repository with
  `runs-on: ubuntu-latest`.
- Unsupported-label workflow shows a queued diagnostic with zero matching
  runners.
- Untrusted pull request run receives no secrets or mask values before approval.
- Drained and revoked runners do not claim or complete new work.
- A job-container network bypass attempt cannot reach direct IP destinations or
  the DigitalOcean metadata service unless explicitly allowlisted.

## Operator Controls

Emergency stop:

```sql
UPDATE actions_site_policy
   SET actions_enabled = false,
       updated_at = now()
 WHERE id = true;
```

After this change, newly matched workflows should be skipped by policy and
already queued jobs should not be claimed by runners. Keep this SQL in the
incident runbook until a site-admin UI exists.

Capacity limits:

- `max_repo_queued_runs` bounds backlog.
- `max_repo_concurrent_jobs` bounds active jobs for one repository.
- `max_owner_concurrent_jobs` bounds active jobs across one user or org owner.
- `actor_trigger_limit_per_hour` bounds trigger spam by a single actor.

These are policy controls, not billing meters. They protect the shared pool
while Actions minute accounting is still future work.

## Relationship To S41k

S41k should follow S41j because it is UI parity:

- Actions sidebar and management placeholders;
- workflow-specific run pages;
- run graph canvas;
- log viewer and annotations;
- caches, runners, and metrics pages.

None of those replace S41j's security gates. S41k can make unsupported labels,
queue state, runner health, and usage easier to see, but it should not be the
first line of defense for arbitrary code execution.
