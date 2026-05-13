// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

// SubjectKind discriminates which side of the polymorphic billing
// schema a row applies to. Mirrors the `billing_subject_kind` enum
// from migration 0074.
//
// PRO02 Q1 ratified the hybrid table strategy: `billing_invoices`
// and `billing_webhook_events` carry `(subject_kind, subject_id)`;
// `org_billing_states` and `user_billing_states` remain
// kind-specific. Go callers that need to thread the kind through
// (webhook routing, polymorphic invoice queries) use the constants
// below.
//
// PRO04 builds the `Principal{Kind, ID}` abstraction on top of
// these; PRO03 ships the constants alone so the queries it adds in
// billing.sql have a typed Go binding without yet depending on
// PRO04's larger Principal refactor.
type SubjectKind string

const (
	SubjectKindUser SubjectKind = "user"
	SubjectKindOrg  SubjectKind = "org"
)

// String satisfies fmt.Stringer for log fields without forcing
// callers to cast.
func (k SubjectKind) String() string { return string(k) }

// Valid reports whether the kind is one of the known enum values.
// Used at sqlc-binding sites to guard against zero-value bugs
// before they reach the database (which would surface as a clearer
// but later "invalid input value for enum" error).
func (k SubjectKind) Valid() bool {
	switch k {
	case SubjectKindUser, SubjectKindOrg:
		return true
	}
	return false
}
