// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"testing"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
)

func TestStripePrincipalFromMetadataPRO04Keys(t *testing.T) {
	t.Parallel()
	m := map[string]string{
		stripebilling.MetadataSubjectKind: "user",
		stripebilling.MetadataSubjectID:   "42",
	}
	p, ok := stripePrincipalFromMetadata(m)
	if !ok {
		t.Fatal("expected ok=true for valid PRO04 metadata")
	}
	if !p.IsUser() || p.ID != 42 {
		t.Errorf("user principal mismatch: %+v", p)
	}
}

func TestStripePrincipalFromMetadataOrgKind(t *testing.T) {
	t.Parallel()
	m := map[string]string{
		stripebilling.MetadataSubjectKind: "org",
		stripebilling.MetadataSubjectID:   "7",
	}
	p, ok := stripePrincipalFromMetadata(m)
	if !ok {
		t.Fatal("expected ok=true for org PRO04 metadata")
	}
	if !p.IsOrg() || p.ID != 7 {
		t.Errorf("org principal mismatch: %+v", p)
	}
}

func TestStripePrincipalFromMetadataRejectsMissingKeys(t *testing.T) {
	t.Parallel()
	cases := []map[string]string{
		nil,
		{},
		// Missing id.
		{stripebilling.MetadataSubjectKind: "user"},
		// Missing kind.
		{stripebilling.MetadataSubjectID: "1"},
		// Bogus kind.
		{
			stripebilling.MetadataSubjectKind: "alien",
			stripebilling.MetadataSubjectID:   "1",
		},
		// Non-integer id.
		{
			stripebilling.MetadataSubjectKind: "user",
			stripebilling.MetadataSubjectID:   "abc",
		},
		// Zero/negative id.
		{
			stripebilling.MetadataSubjectKind: "user",
			stripebilling.MetadataSubjectID:   "0",
		},
		{
			stripebilling.MetadataSubjectKind: "user",
			stripebilling.MetadataSubjectID:   "-1",
		},
	}
	for i, m := range cases {
		if _, ok := stripePrincipalFromMetadata(m); ok {
			t.Errorf("case %d: expected ok=false for %+v", i, m)
		}
	}
}

func TestStripeOrgIDFromMetadataLegacyKey(t *testing.T) {
	t.Parallel()
	m := map[string]string{stripebilling.MetadataOrgID: "99"}
	if got := stripeOrgIDFromMetadata(m); got != 99 {
		t.Errorf("legacy org id: got %d, want 99", got)
	}
	// Missing / bad ones return 0.
	for _, m := range []map[string]string{
		nil,
		{},
		{stripebilling.MetadataOrgID: ""},
		{stripebilling.MetadataOrgID: "abc"},
		{stripebilling.MetadataOrgID: "0"},
		{stripebilling.MetadataOrgID: "-1"},
	} {
		if got := stripeOrgIDFromMetadata(m); got != 0 {
			t.Errorf("expected 0 for %+v, got %d", m, got)
		}
	}
}

// Make sure the resolver chain prefers PRO04 keys over legacy when
// both are present — defends against an old org's metadata being
// re-stamped during Pro adoption.
func TestStripePrincipalPRO04KeysPreferredOverLegacy(t *testing.T) {
	t.Parallel()
	m := map[string]string{
		stripebilling.MetadataSubjectKind: "user",
		stripebilling.MetadataSubjectID:   "42",
		stripebilling.MetadataOrgID:       "7", // would route to org if used
	}
	p, ok := stripePrincipalFromMetadata(m)
	if !ok || !p.IsUser() || p.ID != 42 {
		t.Errorf("PRO04 user keys not preferred: got %+v ok=%v", p, ok)
	}
}

// Mismatch guard: when the orchestrator constructs a Principal,
// the kind and id must survive String() formatting (used in logs).
func TestPrincipalStringRoundTrip(t *testing.T) {
	t.Parallel()
	if got := orgbilling.PrincipalForUser(99).String(); got != "user:99" {
		t.Errorf("user string: %q", got)
	}
	if got := orgbilling.PrincipalForOrg(99).String(); got != "org:99" {
		t.Errorf("org string: %q", got)
	}
}
