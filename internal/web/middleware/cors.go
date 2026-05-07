// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"strings"
)

// CORSConfig holds the strict CORS policy. By default no cross-origin
// requests are permitted; route groups that need CORS register an explicit
// whitelist (typically just the API surface in S08).
type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	MaxAge         int
}

// CORS returns middleware that enforces the policy. Requests with an
// `Origin` header are checked against AllowedOrigins; OPTIONS preflights
// short-circuit with the appropriate response headers.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	allowedOrigin := func(o string) bool {
		for _, a := range cfg.AllowedOrigins {
			if a == "*" || a == o {
				return true
			}
		}
		return false
	}
	allowedMethods := strings.Join(cfg.AllowedMethods, ", ")
	allowedHeaders := strings.Join(cfg.AllowedHeaders, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !allowedOrigin(origin) {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				if allowedMethods != "" {
					h.Set("Access-Control-Allow-Methods", allowedMethods)
				}
				if allowedHeaders != "" {
					h.Set("Access-Control-Allow-Headers", allowedHeaders)
				}
				if cfg.MaxAge > 0 {
					h.Set("Access-Control-Max-Age", itoa(cfg.MaxAge))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func itoa(i int) string {
	const buflen = 11
	var buf [buflen]byte
	pos := buflen
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
