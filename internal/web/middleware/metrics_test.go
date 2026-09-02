// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

// routeSeries returns shithub_http_requests_total summed per `route`
// label — one map entry per distinct series family, which is exactly
// the cardinality the fix is about.
func routeSeries(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string]float64)
	for _, f := range families {
		if f.GetName() != "shithub_http_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "route" {
					out[l.GetValue()] += m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// TestMetrics_UnmatchedRoutesShareOneSeries is the regression guard
// for the cardinality leak: labelling unrouted requests by raw path
// minted a permanent Prometheus series per scanner probe (11.7k of
// them in production). Three different unmatched paths must land on
// one series.
//
// Not parallel: it reads the process-wide metrics registry.
func TestMetrics_UnmatchedRoutesShareOneSeries(t *testing.T) {
	before := routeSeries(t)

	r := chi.NewRouter()
	r.Use(Metrics)
	// chi short-circuits to NotFoundHandler *before* the middleware
	// chain when the mux carries no routes at all, so register one.
	r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	probes := []string{
		"/wp-login.php",
		"/.env.backup.2026",
		"/vendor/phpunit/eval-stdin.php",
	}
	for _, path := range probes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	after := routeSeries(t)
	if got := after[unmatchedRoute] - before[unmatchedRoute]; got != float64(len(probes)) {
		t.Errorf("%q counter delta = %v, want %d", unmatchedRoute, got, len(probes))
	}
	for _, path := range probes {
		if _, leaked := after[path]; leaked {
			t.Errorf("raw path %q became a metric label", path)
		}
	}
	if n := countNew(before, after); n > 1 {
		t.Errorf("unmatched requests added %d new route series, want at most 1", n)
	}
}

func countNew(before, after map[string]float64) int {
	n := 0
	for label := range after {
		if _, seen := before[label]; !seen {
			n++
		}
	}
	return n
}

// A matched route still gets its chi pattern, not the fallback —
// the fix must not flatten real routes.
func TestMetrics_MatchedRouteUsesChiPattern(t *testing.T) {
	before := routeSeries(t)

	r := chi.NewRouter()
	r.Use(Metrics)
	r.Get("/{owner}/{repo}/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, path := range []string{"/alice/hammer/probe", "/bob/anvil/probe"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	after := routeSeries(t)
	const want = "/{owner}/{repo}/probe"
	if got := after[want] - before[want]; got != 2 {
		t.Errorf("%q counter delta = %v, want 2", want, got)
	}
	if n := countNew(before, after); n != 1 {
		t.Errorf("added %d new route series, want 1", n)
	}
}

// The middleware also runs on the bare mux in some wirings, where
// there is no chi route context at all. That path must use the same
// bounded label rather than the raw URL.
func TestMetrics_NoChiContextUsesUnmatched(t *testing.T) {
	before := routeSeries(t)

	h := Metrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	const path = "/some/unrouted/path"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))

	after := routeSeries(t)
	if got := after[unmatchedRoute] - before[unmatchedRoute]; got != 1 {
		t.Errorf("%q counter delta = %v, want 1", unmatchedRoute, got)
	}
	if _, leaked := after[path]; leaked {
		t.Errorf("raw path %q became a metric label", path)
	}
}
