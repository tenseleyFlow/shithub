// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// AccessLog returns middleware that emits one structured log line per
// request after the response is written. Includes request_id when present.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := newStatusRecorder(w)
			next.ServeHTTP(rec, r)
			elapsed := time.Since(start)
			logger.LogAttrs(
				r.Context(), slog.LevelInfo, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", elapsed),
				slog.String("remote_ip", RealIPFromContext(r.Context(), r)),
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// statusRecorder is a small wrapper that captures the status code and
// response size for access logging without buffering the body.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when supported, so streaming
// responses (git protocol later) keep working through the access-log
// wrapper. We do NOT wrap Hijacker since none of S02's handlers need it.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController
// can reach the conn for SetWriteDeadline / SetReadDeadline. Without
// this, the git smart-HTTP handler's per-request deadline clear (added
// in firedrill v3) silently no-ops because NewResponseController walks
// the chain via Unwrap and stops at our wrapper. The push then dies at
// the http.Server's 30s WriteTimeout. Firedrill v4, 2026-05-25.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
