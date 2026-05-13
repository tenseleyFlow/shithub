// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

import (
	"errors"
	"testing"
)

func TestPrincipalValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Principal
		want error
	}{
		{"zero value", Principal{}, ErrInvalidPrincipal},
		{"zero kind", Principal{Kind: "", ID: 1}, ErrInvalidPrincipal},
		{"bogus kind", Principal{Kind: "alien", ID: 1}, ErrInvalidPrincipal},
		{"zero id", Principal{Kind: SubjectKindUser, ID: 0}, ErrInvalidPrincipal},
		{"negative id", Principal{Kind: SubjectKindOrg, ID: -42}, ErrInvalidPrincipal},
		{"valid user", PrincipalForUser(7), nil},
		{"valid org", PrincipalForOrg(7), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.want == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestPrincipalKindPredicates(t *testing.T) {
	t.Parallel()
	if !PrincipalForUser(1).IsUser() || PrincipalForUser(1).IsOrg() {
		t.Errorf("PrincipalForUser predicates wrong")
	}
	if !PrincipalForOrg(1).IsOrg() || PrincipalForOrg(1).IsUser() {
		t.Errorf("PrincipalForOrg predicates wrong")
	}
}

func TestPrincipalString(t *testing.T) {
	t.Parallel()
	if got, want := PrincipalForUser(42).String(), "user:42"; got != want {
		t.Errorf("user String: got %q, want %q", got, want)
	}
	if got, want := PrincipalForOrg(7).String(), "org:7"; got != want {
		t.Errorf("org String: got %q, want %q", got, want)
	}
}
