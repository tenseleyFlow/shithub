// SPDX-License-Identifier: AGPL-3.0-or-later

package advisorymatch

import "testing"

func TestMatchVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		ecosystem string
		version   string
		expr      string
		want      bool
	}{
		{
			name:      "empty range matches current manual advisories",
			ecosystem: "go",
			version:   "v1.2.3",
			expr:      "",
			want:      true,
		},
		{
			name:      "go exact version ignores v prefix differences",
			ecosystem: "go",
			version:   "v1.2.3",
			expr:      "1.2.3",
			want:      true,
		},
		{
			name:      "go comparison range matches",
			ecosystem: "go",
			version:   "v1.2.3",
			expr:      ">= v1.0.0, < v1.2.4",
			want:      true,
		},
		{
			name:      "go comparison range excludes fixed version",
			ecosystem: "go",
			version:   "v1.2.4",
			expr:      ">= v1.0.0, < v1.2.4",
			want:      false,
		},
		{
			name:      "go pseudo versions compare by prerelease identifiers",
			ecosystem: "go",
			version:   "v0.0.0-20240501000000-deadbeef",
			expr:      "< v0.0.0-20240601000000-feedface",
			want:      true,
		},
		{
			name:      "npm caret range matches same major",
			ecosystem: "npm",
			version:   "1.4.0",
			expr:      "^1.2.3",
			want:      true,
		},
		{
			name:      "npm caret range excludes next major",
			ecosystem: "npm",
			version:   "2.0.0",
			expr:      "^1.2.3",
			want:      false,
		},
		{
			name:      "npm zero-major caret stays within minor",
			ecosystem: "npm",
			version:   "0.3.0",
			expr:      "^0.2.3",
			want:      false,
		},
		{
			name:      "npm tilde range matches patch window",
			ecosystem: "npm",
			version:   "1.2.9",
			expr:      "~1.2.3",
			want:      true,
		},
		{
			name:      "npm tilde range excludes next minor",
			ecosystem: "npm",
			version:   "1.3.0",
			expr:      "~1.2.3",
			want:      false,
		},
		{
			name:      "npm wildcard range matches",
			ecosystem: "npm",
			version:   "1.2.9",
			expr:      "1.2.x",
			want:      true,
		},
		{
			name:      "npm wildcard range excludes next minor",
			ecosystem: "npm",
			version:   "1.3.0",
			expr:      "1.2.x",
			want:      false,
		},
		{
			name:      "hyphen range is inclusive",
			ecosystem: "npm",
			version:   "1.5.0",
			expr:      "1.2.0 - 1.5.0",
			want:      true,
		},
		{
			name:      "or range matches either alternative",
			ecosystem: "npm",
			version:   "2.2.0",
			expr:      "<1.0.0 || >=2.0.0 <2.3.0",
			want:      true,
		},
		{
			name:      "unresolved manifest spec does not satisfy range",
			ecosystem: "npm",
			version:   "^1.2.3",
			expr:      "< 2.0.0",
			want:      false,
		},
		{
			name:      "unsupported ecosystem keeps exact fallback",
			ecosystem: "pip",
			version:   "1.2.3",
			expr:      "<2.0.0",
			want:      false,
		},
		{
			name:      "prerelease sorts below release",
			ecosystem: "npm",
			version:   "1.2.3-beta.1",
			expr:      "<1.2.3",
			want:      true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := MatchVersion(tt.ecosystem, tt.version, tt.expr)
			if got != tt.want {
				t.Fatalf("MatchVersion(%q, %q, %q) = %v, want %v", tt.ecosystem, tt.version, tt.expr, got, tt.want)
			}
		})
	}
}
