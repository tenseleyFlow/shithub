// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

// Metrics returns middleware that records HTTP-level metrics via the
// project-wide Prometheus registry.
//
// Route labels are extracted from chi when available so we get
// "/owner/{repo}" instead of the per-repo concrete path — keeping
// cardinality bounded.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metrics.HTTPInFlight.Inc()
		defer metrics.HTTPInFlight.Dec()

		start := time.Now()
		rec := newStatusRecorder(w)
		next.ServeHTTP(rec, r)

		route := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			route = rctx.RoutePattern()
		}
		method := r.Method
		status := strconv.Itoa(rec.status)
		metrics.HTTPRequestsTotal.WithLabelValues(route, method, status).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
	})
}
