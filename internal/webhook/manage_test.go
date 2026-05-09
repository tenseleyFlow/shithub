// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEvents(t *testing.T) {
	in := []string{"Push", " issues ", "push", ""}
	got, err := normalizeEvents(in)
	if err != nil {
		t.Fatalf("normalizeEvents err = %v", err)
	}
	want := []string{"push", "issues"}
	if len(got) != len(want) {
		t.Fatalf("normalizeEvents len = %d; want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalizeEvents[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeEventsRejectsTooLong(t *testing.T) {
	long := strings.Repeat("a", 65)
	if _, err := normalizeEvents([]string{long}); !errors.Is(err, ErrBadEvent) {
		t.Fatalf("normalizeEvents(long) err = %v; want ErrBadEvent", err)
	}
}

func TestValidateURLAcceptsHTTPS(t *testing.T) {
	if err := validateURL("https://example.com/x"); err != nil {
		t.Fatalf("validateURL(good) = %v", err)
	}
}

func TestValidateURLRejectsBadShapes(t *testing.T) {
	cases := []string{
		"",
		"file:///etc/passwd",
		"http://",
		"//example.com/x",
	}
	for _, raw := range cases {
		if err := validateURL(raw); !errors.Is(err, ErrBadURL) {
			t.Errorf("validateURL(%q) = %v; want ErrBadURL", raw, err)
		}
	}
}
