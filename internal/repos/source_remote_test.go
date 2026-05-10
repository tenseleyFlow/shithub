// SPDX-License-Identifier: AGPL-3.0-or-later

package repos_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos"
)

func TestNormalizeSourceRemoteURL(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "https", raw: " https://github.com/FortranGoingOnForty/fgof-process.git ", want: "https://github.com/FortranGoingOnForty/fgof-process.git"},
		{name: "http", raw: "HTTP://git.example.com/owner/repo.git", want: "http://git.example.com/owner/repo.git"},
		{name: "empty clears", raw: " ", want: ""},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := repos.NormalizeSourceRemoteURL(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeSourceRemoteURL(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeSourceRemoteURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeSourceRemoteURLRejectsUnsafeShapes(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"git@github.com:owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
		"file:///tmp/repo.git",
		"https://user:pass@example.com/owner/repo.git",
		"https://example.com",
		"https://example.com/owner/repo.git?token=secret",
		"https://example.com/owner/repo.git#main",
		strings.Repeat("a", repos.MaxSourceRemoteURLLen+1),
	} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if got, err := repos.NormalizeSourceRemoteURL(raw); !errors.Is(err, repos.ErrInvalidSourceRemote) {
				t.Fatalf("NormalizeSourceRemoteURL(%q) = %q, %v; want ErrInvalidSourceRemote", raw, got, err)
			}
		})
	}
}
