// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

import (
	"errors"
	"fmt"
	"strconv"
)

// Principal is the routing key for the polymorphic billing surface.
// PRO02 Q3 ratified the abstraction: every billing service function
// that takes a subject takes a Principal, and the service-layer
// internals branch to the org or user sqlc query based on Kind.
//
// PRO04 lands the type and uses it internally — the existing
// org-shaped service wrappers (`ApplySubscriptionSnapshot(...snap)`
// where snap embeds OrgID) build a Principal{SubjectKindOrg,
// snap.OrgID} and call the Principal-taking sibling. PRO05 sweeps
// the handler-side gating call sites to use Principal directly.
//
// The zero value (`Principal{}`) is invalid by design: a routing
// key that doesn't say where to route silently is a bug, not a
// default.
type Principal struct {
	Kind SubjectKind
	ID   int64
}

// ErrInvalidPrincipal surfaces from `Validate` and is returned by
// any billing service function called with a zero-value or otherwise
// malformed Principal. Routes that reach the database with a bad
// Principal would surface a less-clear "invalid input value for
// enum" error from postgres — checking up front is friendlier.
var ErrInvalidPrincipal = errors.New("billing: invalid principal")

// PrincipalForOrg is the canonical constructor for the org branch.
// Equivalent to `Principal{Kind: SubjectKindOrg, ID: orgID}` but
// the named constructor reads better at call sites and prevents
// the kind/id field-order mix-up that a struct literal allows.
func PrincipalForOrg(orgID int64) Principal {
	return Principal{Kind: SubjectKindOrg, ID: orgID}
}

// PrincipalForUser is the user-side counterpart.
func PrincipalForUser(userID int64) Principal {
	return Principal{Kind: SubjectKindUser, ID: userID}
}

// Validate reports whether `p` is well-formed. Used at service-layer
// entry points to surface bad routing keys with a clear error rather
// than letting them reach the SQL layer.
func (p Principal) Validate() error {
	if !p.Kind.Valid() {
		return fmt.Errorf("%w: kind %q", ErrInvalidPrincipal, p.Kind)
	}
	if p.ID <= 0 {
		return fmt.Errorf("%w: id %d", ErrInvalidPrincipal, p.ID)
	}
	return nil
}

// IsOrg / IsUser are kind-narrowing predicates for call sites that
// want a typed branch without comparing constants directly.
func (p Principal) IsOrg() bool  { return p.Kind == SubjectKindOrg }
func (p Principal) IsUser() bool { return p.Kind == SubjectKindUser }

// String formats `<kind>:<id>` for log fields and error messages.
// Matches the metadata-key convention used in Stripe events
// (`shithub_subject_kind` + `shithub_subject_id`).
func (p Principal) String() string {
	return string(p.Kind) + ":" + strconv.FormatInt(p.ID, 10)
}
