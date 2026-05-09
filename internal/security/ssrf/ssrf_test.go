// SPDX-License-Identifier: AGPL-3.0-or-later

package ssrf

import (
	"net"
	"strings"
	"testing"
)

func TestValidateRejectsBadShapes(t *testing.T) {
	c := Default()
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
				t.Fatalf("Validate(%q) = nil; want *Error", tc.url)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate(%q) = %q; want substring %q", tc.url, err, tc.wantSubstr)
			}
		})
	}
}

func TestValidatePassesGoodShapes(t *testing.T) {
	c := Default()
	for _, u := range []string{
		"http://example.com/x",
		"https://example.com:443/x",
		"http://example.com:8080/y",
		"https://example.com:8443/y",
	} {
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
		"100.64.0.1",
		"169.254.169.254",
		"0.0.0.0",
		"255.255.255.255",
		"::1",
		"fe80::1",
		"fd00::1",
		"fc00::1",
	}
	for _, addr := range forbidden {
		ip := net.ParseIP(addr)
		if !IsForbiddenIP(ip) {
			t.Errorf("IsForbiddenIP(%q) = false; want true", addr)
		}
	}
	for _, addr := range []string{"1.1.1.1", "8.8.8.8", "203.0.113.5", "2001:4860:4860::8888"} {
		ip := net.ParseIP(addr)
		if IsForbiddenIP(ip) {
			t.Errorf("IsForbiddenIP(%q) = true; want false", addr)
		}
	}
}

func TestIs(t *testing.T) {
	if !Is(Default().Validate("file:///etc/passwd")) {
		t.Fatal("Is should match")
	}
	if Is(nil) {
		t.Fatal("Is(nil) should be false")
	}
}
