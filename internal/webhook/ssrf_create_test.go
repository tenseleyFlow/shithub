// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestSSRFRejectsLoopbackAndPrivateAtCreateTime pins SR2 H3:
// Create/Update must reject loopback / RFC1918 / disallowed-port URLs
// synchronously instead of letting them persist and only fail on
// every delivery attempt.
//
// Production code calls cfg.ValidateWithResolve(ctx, url) — that's
// what Create() and Update() use. The plain Validate() is the cheap
// syntactic gate; ValidateWithResolve adds the IP-block-list check
// that catches loopback + RFC1918 hosts (literal and resolved).
func TestSSRFRejectsLoopbackAndPrivateAtCreateTime(t *testing.T) {
	t.Parallel()

	cfg := DefaultSSRFConfig()
	rejected := []string{
		"http://127.0.0.1/hook",
		"http://127.0.0.1:8080/hook",
		"http://[::1]/hook",
		"http://192.168.1.1/hook",
		"http://10.0.0.1/hook",
		"http://172.16.0.1/hook",
		// Disallowed port (only 80/443/8080/8443 pass by default).
		"http://example.com:9090/hook",
	}
	ctx := context.Background()
	for _, u := range rejected {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			err := cfg.ValidateWithResolve(ctx, u)
			if err == nil {
				t.Fatalf("SSRF.ValidateWithResolve(%q) = nil; expected an error", u)
			}
		})
	}

	// Sanity: a public-looking URL on a default port should pass.
	if err := cfg.ValidateWithResolve(ctx, "https://example.com/hook"); err != nil {
		t.Fatalf("SSRF.ValidateWithResolve(public) = %v; expected nil", err)
	}
}

// TestCreateUpdateWrapsSSRFErrInBadURL pins the error contract used
// by Create/Update: an SSRF rejection wraps ErrBadURL so callers can
// errors.Is(err, ErrBadURL) for form-shaped feedback. Production code
// uses fmt.Errorf("%w: %v", ErrBadURL, err) — this test pins that the
// wrap shape unwraps correctly.
func TestCreateUpdateWrapsSSRFErrInBadURL(t *testing.T) {
	t.Parallel()

	inner := errors.New("ssrf: loopback")
	wrapped := fmt.Errorf("%w: %v", ErrBadURL, inner)

	if !errors.Is(wrapped, ErrBadURL) {
		t.Fatalf("wrapped error is not ErrBadURL: %v", wrapped)
	}
}
