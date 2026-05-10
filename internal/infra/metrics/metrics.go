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
		ActionsRunsEnqueuedTotal,
		ActionsTriggerMatchDurationSeconds,
	)
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
