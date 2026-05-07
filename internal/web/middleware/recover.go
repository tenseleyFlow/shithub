// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/tenseleyFlow/shithub/internal/infra/errrep"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

// PanicHandler renders a styled error response when the request handler
// panics. The error page receives the request_id so users can quote it for
// support.
type PanicHandler interface {
	HandlePanic(w http.ResponseWriter, r *http.Request, requestID string, recovered any)
}

// Recover returns middleware that catches panics, logs them with the
// request_id, and delegates rendering to handler. If handler is nil a plain
// "internal server error" body is written.
func Recover(logger *slog.Logger, handler PanicHandler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					metrics.PanicsTotal.Inc()
					reqID := RequestIDFromContext(r.Context())
					if logger != nil {
						logger.ErrorContext(
							r.Context(),
							"panic in handler",
							slog.String("request_id", reqID),
							slog.Any("panic", rec),
							slog.String("stack", string(debug.Stack())),
						)
					}
					errrep.CapturePanic(rec, reqID)
					if handler != nil {
						handler.HandlePanic(w, r, reqID, rec)
						return
					}
					if w.Header().Get("Content-Type") == "" {
						w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					}
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = fmt.Fprintf(w, "internal server error (request_id=%s)\n", reqID)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
