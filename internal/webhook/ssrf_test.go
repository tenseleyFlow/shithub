// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"net"
	"strings"
	"testing"
)

func TestValidateRejectsBadShapes(t *testing.T) {
	c := DefaultSSRFConfig()
	cases := []struct {
		name, url, wantSubstr string
	}{
		{"empty", "", "scheme  not allowed"},
		{"file scheme", "file:///etc/passwd", "scheme file not allowed"},
		{"ftp scheme", "ftp://example.com/", "scheme ftp not allowed"},
		{"missing host", "http:///path", "missing host"},
		{"non-allowed port", "http://example.com:9999/x", "port 9999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Validate(tc.url)
			if err == nil {
				t.Fatalf("Validate(%q) = nil; want SSRFError", tc.url)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate(%q) = %q; want substring %q", tc.url, err, tc.wantSubstr)
			}
		})
	}
}

func TestValidatePassesGoodShapes(t *testing.T) {
	c := DefaultSSRFConfig()
	cases := []string{
		"http://example.com/x",
		"https://example.com:443/x",
		"http://example.com:8080/y",
		"https://example.com:8443/y",
	}
	for _, u := range cases {
		if err := c.Validate(u); err != nil {
			t.Fatalf("Validate(%q) = %v; want nil", u, err)
		}
	}
}

func TestIsForbiddenIPClassifiesCorrectly(t *testing.T) {
	forbidden := []string{
		"127.0.0.1", "127.255.255.254",
		"10.0.0.1", "10.255.255.255",
		"172.16.0.1", "172.31.255.255",
		"192.168.0.1",
		"100.64.0.1",      // CGNAT
		"169.254.169.254", // AWS metadata service
		"0.0.0.0",
		"255.255.255.255",
		"::1",
		"fe80::1", // link-local
		"fd00::1", // ULA
		"fc00::1", // ULA
	}
	for _, addr := range forbidden {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("bad test fixture: %q", addr)
		}
		if !isForbiddenIP(ip) {
			t.Errorf("isForbiddenIP(%q) = false; want true", addr)
		}
	}
	allowed := []string{
		"1.1.1.1", "8.8.8.8", "203.0.113.5", "198.51.100.7",
		"2001:4860:4860::8888",
	}
	for _, addr := range allowed {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("bad test fixture: %q", addr)
		}
		if isForbiddenIP(ip) {
			t.Errorf("isForbiddenIP(%q) = true; want false", addr)
		}
	}
}

func TestIsSSRF(t *testing.T) {
	c := DefaultSSRFConfig()
	err := c.Validate("file:///etc/passwd")
	if !IsSSRF(err) {
		t.Fatalf("IsSSRF(%v) = false; want true", err)
	}
	if IsSSRF(nil) {
		t.Fatalf("IsSSRF(nil) = true; want false")
	}
}
