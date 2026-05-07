// SPDX-License-Identifier: AGPL-3.0-or-later

// Package middleware provides shithub's HTTP middleware stack. Composable
// with chi/net.http; each middleware exposes its own constructor.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestIDKey is the context key under which the request_id is stored.
type ctxKey struct{ name string }

func (c ctxKey) String() string { return "shithub:" + c.name }

var requestIDKey = ctxKey{name: "request_id"}

// RequestIDHeader is the canonical incoming/outgoing request-ID header.
const RequestIDHeader = "X-Request-Id"

// RequestID returns middleware that ensures every request has a request_id.
// If the inbound request carries an X-Request-Id header it's reused (capped
// to a sane length); otherwise we generate a 16-byte random hex string. The
// id is propagated via context and echoed in the response headers so a
// caller can correlate logs with their own traces.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !isValidRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request_id, or "" if none is set.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// isValidRequestID accepts hex/dash/colon/slash separators in the [a-z0-9]
// alphabet, capped at 128 chars. We don't trust client-supplied tracing
// values blindly but we also don't want to throw away genuine ones.
func isValidRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '-' || c == '_' || c == '/' || c == ':' || c == '.':
		default:
			return false
		}
	}
	return true
}
