// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicBaseURLRejectsUnsafeRequestHostFallback(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Host = "example.com\r\nSitemap: https://evil.test/sitemap.xml"
	if got := publicBaseURL("", req); got != "" {
		t.Fatalf("publicBaseURL accepted unsafe host = %q", got)
	}
}

func TestPublicBaseURLPrefersConfiguredBase(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Host = "untrusted.example"
	if got, want := publicBaseURL("https://shithub.sh/", req), "https://shithub.sh"; got != want {
		t.Fatalf("publicBaseURL = %q, want %q", got, want)
	}
}
