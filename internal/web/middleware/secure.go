// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import "net/http"

// SecureHeadersConfig lets the operator override defaults. Values left as
// the zero string fall back to the documented defaults.
type SecureHeadersConfig struct {
	CSP             string // Content-Security-Policy
	HSTS            string // Strict-Transport-Security; only set when TLS
	ReferrerPolicy  string
	PermissionsPol  string
	FrameOptions    string
	ContentTypeOpts string
	COOP            string
	CORP            string
}

// DefaultSecureHeaders returns the policy applied to web responses. S35
// will lock these down further; S02's defaults are the practical baseline.
func DefaultSecureHeaders() SecureHeadersConfig {
	return SecureHeadersConfig{
		// Permit Primer CSS's inline style attributes (it uses them
		// liberally) and our own first-party scripts. S35 evaluates moving
		// to nonces / strict-dynamic.
		//
		// form-action allows Stripe's hosted Checkout and Customer billing
		// portal hosts so the POST→303→Stripe redirect chain isn't blocked
		// by browsers that enforce form-action across redirects (Safari,
		// recent Chromium). Without those entries Safari aborts the
		// redirect from /settings/billing/checkout to checkout.stripe.com
		// with no visible error.
		CSP: "default-src 'self'; " +
			"img-src 'self' data: https:; " +
			"style-src 'self' 'unsafe-inline'; " +
			"script-src 'self' 'unsafe-inline'; " +
			"font-src 'self' data:; " +
			"connect-src 'self'; " +
			"frame-ancestors 'none'; " +
			"form-action 'self' https://checkout.stripe.com https://billing.stripe.com; " +
			"base-uri 'self'; " +
			"object-src 'none'",
		HSTS:            "max-age=31536000; includeSubDomains",
		ReferrerPolicy:  "strict-origin-when-cross-origin",
		PermissionsPol:  "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()",
		FrameOptions:    "DENY",
		ContentTypeOpts: "nosniff",
		COOP:            "same-origin",
		CORP:            "same-origin",
	}
}

// SecureHeaders returns middleware that stamps the configured security
// headers on every response. HSTS is only set when the request reached us
// over TLS or via a trusted X-Forwarded-Proto=https proxy header.
func SecureHeaders(cfg SecureHeadersConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			if cfg.CSP != "" {
				h.Set("Content-Security-Policy", cfg.CSP)
			}
			if cfg.ReferrerPolicy != "" {
				h.Set("Referrer-Policy", cfg.ReferrerPolicy)
			}
			if cfg.PermissionsPol != "" {
				h.Set("Permissions-Policy", cfg.PermissionsPol)
			}
			if cfg.FrameOptions != "" {
				h.Set("X-Frame-Options", cfg.FrameOptions)
			}
			if cfg.ContentTypeOpts != "" {
				h.Set("X-Content-Type-Options", cfg.ContentTypeOpts)
			}
			if cfg.COOP != "" {
				h.Set("Cross-Origin-Opener-Policy", cfg.COOP)
			}
			if cfg.CORP != "" {
				h.Set("Cross-Origin-Resource-Policy", cfg.CORP)
			}
			if cfg.HSTS != "" && requestIsTLS(r) {
				h.Set("Strict-Transport-Security", cfg.HSTS)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}
