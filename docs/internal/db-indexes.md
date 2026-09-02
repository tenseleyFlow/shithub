# DB indexes — catalog

Per-domain catalog of every non-PK index, the query it backs, and the
rationale. Each new index needs an entry here AND a sentence in its
migration file justifying the write-throughput cost. Reviewed on
every perf-pass sprint (S36 baseline; revisited on S39 beta hardening).

The columns are: **Index**, **Covers query**, **Selectivity**, **Cost
notes**. Selectivity is rough — "high" means typically returns ≤ 100
rows for a 1M-row table; "medium" 1–10k; "low" most-of-the-table.

## Identity (`users`, `user_emails`, `user_*`)

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `users.username_uq` (unique) | login + profile lookup by username | high | implied by UNIQUE; PK-class |
| `users_deleted_at_idx` (partial WHERE deleted_at IS NOT NULL) | restore listing + admin "deleted" filter | high | partial keeps it tiny |
| `users_suspended_at_idx` (partial WHERE suspended_at IS NOT NULL) | admin "suspended" filter | high | partial |
| `users_site_admin_idx` (partial WHERE is_site_admin = true) | admin elevation lookups | high | partial; few rows |
| `user_emails (user_id)` | enumerate a user's emails | high | per-user |
| `user_emails_address_uq` (unique) | reverse lookup on signup-collision check | high | UNIQUE |

## Repos

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `repos (owner_user_id, name)` PK-class | per-owner repo lookup | high | composite |
| `repos_owner_org_name_uq` | org-owner repo lookup | high | UNIQUE |
| `repo_collaborators_user_id_idx` | "all my repos" enumeration | medium | parallel index to PK |
| `repo_collaborators_repo_id_idx` | settings access tab | high | per-repo |
| `repo_topics_topic_idx` | "repos with topic X" | medium | post-MVP topic filter |

## Issues / pulls

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `issues (repo_id, number)` PK | issue-by-number | high | PK |
| `issues_repo_state_updated_idx (repo_id, state, updated_at DESC)` | issues list with state filter (S21) | medium | the dominant list-page index |
| `issues_repo_kind_state_idx (repo_id, kind, state)` | PR-vs-issue split | medium | composite |
| `issues_author_idx (author_user_id)` | "issues authored by me" | medium | per-user-author |
| `issues_search_*` | FTS lookup | medium | GIN tsv index |

## Jobs queue

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `jobs_dispatch_idx (kind, run_at) WHERE completed_at IS NULL AND failed_at IS NULL` | worker claim — hot path | high | partial; only dispatchable rows |
| `jobs_locked_idx (locked_by, locked_at)` | stuck-claim sweeper | high | rare scan |

## Notifications + domain events

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `notifications_recipient_recent_idx (recipient_user_id, last_event_at DESC)` | inbox first page | high | per-user, recency-sorted |
| `notifications_recipient_unread_idx` | unread badge | high | partial |
| `notifications_thread_coalesce_idx` (UNIQUE) | dedup thread rows on fanout | high | UNIQUE |
| `domain_events_created_at_idx (created_at)` | fanout cursor scan | low | covers polling |
| `domain_events_repo_created_idx (repo_id, created_at DESC) WHERE repo_id IS NOT NULL` | per-repo activity feed | medium | partial |
| `domain_events_actor_created_idx (actor_user_id, created_at DESC) WHERE actor_user_id IS NOT NULL` | per-user activity feed | medium | partial |
| `domain_events_public_created_idx (created_at DESC) WHERE public = true` | public feed | medium | partial |

## Check runs

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `check_runs_repo_head_idx (repo_id, head_sha)` | required-check eval per PR | high | composite |
| `check_runs_required_lookup_idx (repo_id, head_sha, name)` | named-check exact lookup | high | composite |
| `check_runs_external_id_idx` (UNIQUE) | upsert-by-(name+external_id) | high | UNIQUE |

## Auth audit log

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `auth_audit_log_actor_id_idx` | "what did actor X do" | medium | per-actor |
| `auth_audit_log_target_idx (target_type, target_id)` | "what happened to target Y" | medium | composite |
| `auth_audit_log_action_idx` | filter by action prefix | medium | per-action (admin viewer) |
| `auth_audit_log_created_at_idx (created_at DESC)` | recency scan | low | dominant index for the admin viewer |

## Webhooks (S33)

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `webhooks_owner_idx (owner_kind, owner_id)` | settings list | high | composite |
| `webhooks_active_idx (owner_kind, owner_id) WHERE active = true AND disabled_at IS NULL` | fanout subscriber lookup | high | partial; the hot path |
| `webhook_deliveries_pending_due_idx (next_retry_at) WHERE status IN ('pending','failed_retry')` | deliverer claim | high | partial |
| `webhook_deliveries_webhook_started_idx (webhook_id, started_at DESC)` | per-webhook delivery view | medium | composite |

## Rate-limit (S35)

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `rate_limits` PK `(scope, key)` | per-scope-key bump | high | UPSERT hot path |
| `rate_limits_window_started_idx (window_started_at)` | periodic prune | low | scan-friendly |
| `signup_ip_throttle` PK `(cidr)` | per-/24 lookup | high | UPSERT |
| `signup_ip_throttle_window_started_idx (window_started_at)` | periodic prune | low | scan-friendly |

## Repo traffic (S38 + retention, availability campaign)

| Index | Covers query | Selectivity | Cost notes |
|---|---|---|---|
| `repo_traffic_daily` PK `(repo_id, day)` | Traffic chart for one repo | high | UPSERT hot path |
| `repo_traffic_daily_day_idx (day DESC)` | 400-day retention purge | low | scan-friendly |
| `repo_traffic_paths` PK `(repo_id, day, path)` | popular-content rollup | high | UPSERT hot path |
| `repo_traffic_paths_day_idx (day)` | 30-day retention purge | low | 0127; without it every purge batch is a seq scan |
| `repo_traffic_referrers` PK `(repo_id, day, referrer)` | referrer rollup | high | UPSERT hot path |
| `repo_traffic_referrers_day_idx (day)` | 30-day retention purge | low | 0127 |
| `repo_traffic_uniques` PK `(repo_id, day, metric, key, visitor_hash)` | dedupe a visitor within a day | high | INSERT ... ON CONFLICT DO NOTHING, one per pageview |
| `repo_traffic_uniques_created_idx (created_at)` | 30-day retention purge | low | 0112; the purge filters this table on `created_at` rather than `day` so it can reuse this index instead of adding a second one to a table that takes an insert per pageview |

The purge (`traffic:purge`) deletes with
`WHERE ctid IN (SELECT ctid FROM <table> WHERE <cutoff column> < $1 LIMIT $2)`.
The `day`/`created_at` index drives the subselect; the outer delete is a
tid scan. Without the index the plan is a sequential scan **per batch**,
which on the production table sizes (1.26 M paths, 1.34 M uniques as of
2026-09-02) is a few hundred full scans per run.

## Future considerations (deferred)

- **`pg_stat_statements` extension.** S37's deploy doc owns the
  install. Once the suite runs against a populated dataset we'll
  capture top-N slow queries here and decide on additional
  indexes.
- **`gin_trgm_ops` indexes** for partial-match search on
  usernames / repo names. The existing FTS covers most needs;
  trigram lookup pays off only if profiling shows an LIKE-prefix
  hot path.
- **Covering indexes for the issue list.** `INCLUDE` columns on
  `issues_repo_state_updated_idx` could turn the page into an
  index-only scan. Defer until the scan proves expensive on the
  100k-issue fixture.
