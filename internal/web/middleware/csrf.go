// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"

	"github.com/justinas/nosurf"
)

// CSRFConfig configures the CSRF middleware.
type CSRFConfig struct {
	// CookieDomain is optional.
	CookieDomain string
	// Secure marks the CSRF cookie Secure (set when serving over TLS).
	Secure bool
	// FailureHandler is invoked when CSRF validation fails. If nil, a
	// minimal 403 plain-text response is written.
	FailureHandler http.Handler
	// ExemptFunc returns true for requests that should bypass CSRF
	// validation. State-changing endpoints authenticated by PAT (S08) and
	// the git protocol (S12/S13) register exemptions here.
	ExemptFunc func(*http.Request) bool
}

// CSRF returns middleware that enforces nosurf's double-submit-cookie
// protection. State-changing methods without a valid token are rejected.
func CSRF(cfg CSRFConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// nosurf wraps the inner handler; the OUTER returned handler is what
		// chi's middleware stack sees.
		ns := nosurf.New(next)
		ns.SetBaseCookie(csrfBaseCookie(cfg))
		if cfg.FailureHandler != nil {
			ns.SetFailureHandler(cfg.FailureHandler)
		}
		if cfg.ExemptFunc != nil {
			ns.ExemptFunc(cfg.ExemptFunc)
		}
		return ns
	}
}

// CSRFTokenForRequest exposes the per-request token to templates.
func CSRFTokenForRequest(r *http.Request) string {
	return nosurf.Token(r)
}

// csrfBaseCookie constructs nosurf's base cookie. Secure is
// operator-controlled per cfg.Secure (S37 enables it under TLS).
//
//nolint:gosec // G124: Secure attribute is intentionally configurable via cfg.Secure.
func csrfBaseCookie(cfg CSRFConfig) http.Cookie {
	return http.Cookie{
		Path:     "/",
		Domain:   cfg.CookieDomain,
		Secure:   cfg.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}
