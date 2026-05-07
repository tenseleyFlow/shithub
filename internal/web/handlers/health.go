// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// healthz returns 200 if the process is alive. No dependency checks.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

// readinessHandler returns a /readyz handler that calls check (when non-nil)
// with a 2-second budget. A nil error returns 200 ready; a non-nil error
// returns 503 with the error reason in the body. When check is nil the
// handler always reports ready (the S00 default for a DB-less boot).
func readinessHandler(check func(context.Context) error, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if check == nil {
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := check(ctx); err != nil {
			if logger != nil {
				logger.Warn("readyz: dependency unhealthy", "error", err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready: " + err.Error() + "\n"))
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
}
