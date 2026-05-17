// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metrics owns the Prometheus registry. Standard metrics are
// instantiated up front; per-package metrics register against the same
// shared registry.
package metrics

import (
	"crypto/subtle"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
)

// Registry is the project-wide Prometheus registry. Subpackages register
// their collectors against this so /metrics has a single source.
var Registry = prometheus.NewRegistry()

// Standard process / Go runtime metrics.
func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// HTTP request metrics. Wired by the HTTP middleware.
var (
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_http_requests_total",
			Help: "Total HTTP requests by route, method, and status.",
		},
		[]string{"route", "method", "status"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "shithub_http_request_duration_seconds",
			Help:    "HTTP request duration distribution by route and method.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2.5, 12),
		},
		[]string{"route", "method"},
	)
	HTTPInFlight = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shithub_http_in_flight",
			Help: "Number of HTTP requests currently in flight.",
		},
	)
	PanicsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shithub_panics_total",
			Help: "Total panics caught by the recover middleware.",
		},
	)
)

// DB pool metrics. Updated periodically by an observer goroutine that the
// caller starts via Observe(pool, interval).
var (
	DBConnsAcquired = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shithub_db_pool_acquired",
			Help: "Postgres connections currently checked out of the pool.",
		},
	)
	DBConnsIdle = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shithub_db_pool_idle",
			Help: "Postgres connections currently idle in the pool.",
		},
	)
	DBConnsTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shithub_db_pool_total",
			Help: "Postgres connections currently held by the pool.",
		},
	)
	DBAcquireWaitDurationTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shithub_db_pool_acquire_wait_seconds_total",
			Help: "Cumulative time clients spent waiting to acquire a Postgres connection.",
		},
	)
)

// Worker metrics. The pool updates these on every dispatch.
var (
	WorkerJobsProcessedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_worker_jobs_processed_total",
			Help: "Worker jobs processed by kind and outcome (ok, retry, failed, poison).",
		},
		[]string{"kind", "outcome"},
	)
	WorkerJobDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "shithub_worker_job_duration_seconds",
			Help:    "Worker handler latency by kind.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2.5, 12),
		},
		[]string{"kind"},
	)
	WorkerInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_worker_in_flight",
			Help: "Worker handler invocations currently in flight by kind.",
		},
		[]string{"kind"},
	)
)

// Billing metrics. Handlers increment edge counters; ObserveBilling refreshes
// DB-backed gauges used by launch-readiness alerts.
var (
	BillingCheckoutSessionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_billing_checkout_sessions_total",
			Help: "Stripe Checkout session creation attempts by subject kind and result.",
		},
		[]string{"subject_kind", "result"},
	)
	BillingPortalSessionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_billing_portal_sessions_total",
			Help: "Stripe Billing Portal session creation attempts by subject kind and result.",
		},
		[]string{"subject_kind", "result"},
	)
	BillingWebhookEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_billing_webhook_events_total",
			Help: "Stripe billing webhook deliveries by event type and result.",
		},
		[]string{"event_type", "result"},
	)
	BillingSeatSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_billing_seat_sync_total",
			Help: "Org billing seat-sync outcomes.",
		},
		[]string{"result"},
	)
	BillingWebhookBacklog = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_billing_webhook_backlog",
			Help: "Billing webhook receipts awaiting successful processing by state.",
		},
		[]string{"state"},
	)
	BillingPastDuePrincipals = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_billing_past_due_principals",
			Help: "Principals in payment-action-needed subscription states by subject kind.",
		},
		[]string{"subject_kind"},
	)
	BillingOrgSeatDrift = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shithub_billing_org_seat_drift",
			Help: "Team organizations whose stored seat usage is stale or exceeds licensed capacity.",
		},
	)
	BillingQuotaOverageOrgs = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_billing_quota_overage_orgs",
			Help: "Organizations currently over a billing quota after plan and site-admin overrides are applied.",
		},
		[]string{"quota"},
	)
)

// Actions trigger pipeline metrics (S41b). Incremented from
// internal/actions/trigger.
var (
	ActionsRunsEnqueuedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_runs_enqueued_total",
			Help: "Total workflow runs enqueued by triggering event kind. Result is 'fresh' for new runs or 'already_exists' when ON CONFLICT noop'd.",
		},
		[]string{"event", "result"},
	)
	ActionsTriggerMatchDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "shithub_actions_trigger_match_duration_seconds",
			Help:    "Wall-clock time spent in the trigger handler discovering + parsing + matching workflows for one triggering event.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2.0, 12),
		},
	)
	ActionsRunnerRegistrationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shithub_actions_runner_registrations_total",
			Help: "Total Actions runners registered through operator tooling.",
		},
	)
	ActionsRunnerHeartbeatsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_runner_heartbeats_total",
			Help: "Total runner heartbeats by result (claimed, no_job, rejected).",
		},
		[]string{"result"},
	)
	ActionsRunnerJWTTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_runner_jwt_total",
			Help: "Total runner job JWT outcomes by result (issued, rejected, replay).",
		},
		[]string{"result"},
	)
	ActionsJobsCancelledTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_jobs_cancelled_total",
			Help: "Total Actions job cancellation requests by reason (user, concurrency, timeout).",
		},
		[]string{"reason"},
	)
	ActionsRunsCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_runs_completed_total",
			Help: "Total terminal Actions workflow runs by event kind and conclusion.",
		},
		[]string{"event", "conclusion"},
	)
	ActionsRunDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "shithub_actions_run_duration_seconds",
			Help:    "Actions workflow run duration from started_at or created_at to completed_at, by event kind and conclusion.",
			Buckets: prometheus.ExponentialBuckets(1, 2.5, 12),
		},
		[]string{"event", "conclusion"},
	)
	ActionsStepsCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_steps_completed_total",
			Help: "Total terminal Actions steps by bounded step type and conclusion.",
		},
		[]string{"step_type", "conclusion"},
	)
	ActionsConcurrencyQueuedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shithub_actions_concurrency_queued_total",
			Help: "Total Actions workflow runs queued behind an older active run in the same concurrency group.",
		},
	)
	ActionsLogScrubReplacementsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_log_scrub_replacements_total",
			Help: "Total exact secret-value replacements performed on Actions log chunks.",
		},
		[]string{"location"},
	)
	ActionsLogChunksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_log_chunks_total",
			Help: "Total Actions log chunks accepted by location.",
		},
		[]string{"location"},
	)
	ActionsLogChunkBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_log_chunk_bytes_total",
			Help: "Total Actions log chunk bytes accepted by location before durable storage.",
		},
		[]string{"location"},
	)
	ActionsRunsPrunedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_actions_runs_pruned_total",
			Help: "Total Actions retention deletions by kind (chunks, blobs, runs, jwt_used).",
		},
		[]string{"kind"},
	)
	ActionsStepTimeoutsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shithub_actions_step_timeouts_total",
			Help: "Total Actions steps reported as timed out by runners.",
		},
	)
	ActionsQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_queue_depth",
			Help: "Current queued Actions workflow items by resource (runs, jobs).",
		},
		[]string{"resource"},
	)
	ActionsQueueDepthByLabels = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_queue_depth_by_labels",
			Help: "Current queued Actions jobs by exact runs-on label expression.",
		},
		[]string{"labels"},
	)
	ActionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_active",
			Help: "Current running Actions workflow items by resource (runs, jobs).",
		},
		[]string{"resource"},
	)
	ActionsJobClaimLatencySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "shithub_actions_job_claim_latency_seconds",
			Help:    "Seconds between job enqueue and runner claim.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2.5, 12),
		},
	)
	ActionsRunnerHeartbeatAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_runner_heartbeat_age_seconds",
			Help: "Seconds since each registered Actions runner last heartbeated. Offline runners that never heartbeated are omitted.",
		},
		[]string{"runner", "status"},
	)
	ActionsRunnerOnline = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_runner_online",
			Help: "Current runner online state by runner (1 online, 0 unavailable).",
		},
		[]string{"runner"},
	)
	ActionsRunnerStaleTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "shithub_actions_runner_stale_total",
			Help: "Current count of non-revoked runners whose heartbeat is past the stale threshold.",
		},
	)
	ActionsRunnerDraining = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_runner_draining",
			Help: "Current runner drain state by runner (1 draining, 0 not draining).",
		},
		[]string{"runner"},
	)
	ActionsRunnerCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_runner_capacity",
			Help: "Configured Actions runner capacity by runner and status.",
		},
		[]string{"runner", "status"},
	)
	ActionsRunnerRevocationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "shithub_actions_runner_revocations_total",
			Help: "Total Actions runner hard revocations performed by operator tooling.",
		},
	)
	ActionsStorageObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_storage_objects",
			Help: "Current durable Actions storage object count by kind.",
		},
		[]string{"kind"},
	)
	ActionsStorageBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "shithub_actions_storage_bytes",
			Help: "Current durable Actions storage byte count by kind.",
		},
		[]string{"kind"},
	)
	// ProGateTotal is the PRO-EXT01-17 campaign-wrap counter. One tick
	// per CheckPrincipalFeature call. Labels:
	//   feature — Feature constant (e.g. "user_actions_secrets")
	//   kind    — billing.SubjectKind ("user" or "org")
	//   outcome — "allow" | "deny"
	// Per-feature soak: operators watch the deny rate trend before
	// flipping the per-feature EnforceConfig.* knob from false → true.
	ProGateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "shithub_pro_gate_total",
			Help: "Per-feature entitlement check outcomes (PRO-EXT01-17).",
		},
		[]string{"feature", "kind", "outcome"},
	)
)

func init() {
	Registry.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		HTTPInFlight,
		PanicsTotal,
		DBConnsAcquired,
		DBConnsIdle,
		DBConnsTotal,
		DBAcquireWaitDurationTotal,
		WorkerJobsProcessedTotal,
		WorkerJobDurationSeconds,
		WorkerInFlight,
		BillingCheckoutSessionsTotal,
		BillingPortalSessionsTotal,
		BillingWebhookEventsTotal,
		BillingSeatSyncTotal,
		BillingWebhookBacklog,
		BillingPastDuePrincipals,
		BillingOrgSeatDrift,
		BillingQuotaOverageOrgs,
		ActionsRunsEnqueuedTotal,
		ActionsTriggerMatchDurationSeconds,
		ActionsRunnerRegistrationsTotal,
		ActionsRunnerHeartbeatsTotal,
		ActionsRunnerJWTTotal,
		ActionsJobsCancelledTotal,
		ActionsRunsCompletedTotal,
		ActionsRunDurationSeconds,
		ActionsStepsCompletedTotal,
		ActionsConcurrencyQueuedTotal,
		ActionsLogScrubReplacementsTotal,
		ActionsLogChunksTotal,
		ActionsLogChunkBytesTotal,
		ActionsRunsPrunedTotal,
		ActionsStepTimeoutsTotal,
		ActionsQueueDepth,
		ActionsQueueDepthByLabels,
		ActionsActive,
		ActionsJobClaimLatencySeconds,
		ActionsRunnerHeartbeatAgeSeconds,
		ActionsRunnerOnline,
		ActionsRunnerStaleTotal,
		ActionsRunnerDraining,
		ActionsRunnerCapacity,
		ActionsRunnerRevocationsTotal,
		ActionsStorageObjects,
		ActionsStorageBytes,
		ProGateTotal,
	)
	// PRO-EXT01-17: install the entitlements observation hook so every
	// CheckPrincipalFeature call lands in ProGateTotal. No-op for
	// processes that pull in metrics but never call into entitlements
	// (the hook is just a function pointer).
	entitlements.SetObserveGate(func(feature, kind, outcome string) {
		ProGateTotal.WithLabelValues(feature, kind, outcome).Inc()
	})
}

// Handler returns the /metrics HTTP handler. When user/pass is set, the
// handler enforces HTTP Basic auth; otherwise it serves unauthenticated
// (S35 will tighten the policy).
//
// DisableCompression: promhttp gzips responses when the scraper sends
// Accept-Encoding: gzip. Alloy 1.16's Prometheus scraper advertises gzip
// but mishandles the Content-Encoding: gzip response (parses raw 0x1f
// magic byte as text, scrape fails with up=0). Bypass at the source —
// /metrics payload is small enough that wire savings are irrelevant.
// Skipping the chi Compress middleware on this route (handlers.go) is
// also necessary but not sufficient; promhttp does its own gzip layer.
func Handler(user, pass string) http.Handler {
	h := promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		Registry:           Registry,
		DisableCompression: true,
	})
	if user == "" && pass == "" {
		return h
	}
	expectedUser := []byte(user)
	expectedPass := []byte(pass)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(gotUser), expectedUser) != 1 ||
			subtle.ConstantTimeCompare([]byte(gotPass), expectedPass) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
