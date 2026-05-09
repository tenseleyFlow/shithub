// SPDX-License-Identifier: AGPL-3.0-or-later

package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// IPKey is the canonical anonymous-request keyer: extracts the
// client IP from r.RemoteAddr or the X-Forwarded-For header. The
// trust-XFF flag should be set ONLY when the deployment runs
// behind a CDN/proxy we control; otherwise an attacker can spoof
// the header and dodge IP-keyed limits.
func IPKey(trustForwarded bool) KeyFunc {
	return func(r *http.Request) string {
		if trustForwarded {
			if v := r.Header.Get("X-Forwarded-For"); v != "" {
				// First entry is the client; downstream proxies append.
				if comma := strings.IndexByte(v, ','); comma > 0 {
					v = v[:comma]
				}
				return "ip:" + strings.TrimSpace(v)
			}
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		return "ip:" + host
	}
}

// ClientIP returns the parsed client IP using the same rules as
// IPKey. Used by signup-throttle's CIDR keying.
func ClientIP(r *http.Request, trustForwarded bool) (netip.Addr, bool) {
	raw := ""
	if trustForwarded {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			if comma := strings.IndexByte(v, ','); comma > 0 {
				v = v[:comma]
			}
			raw = strings.TrimSpace(v)
		}
	}
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			raw = host
		} else {
			raw = r.RemoteAddr
		}
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}
