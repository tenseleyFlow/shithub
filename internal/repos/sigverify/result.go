// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sigverify orchestrates OpenPGP signature verification for
// commits and annotated tags. The shape of the Result type mirrors
// GitHub's documented `verification` object on the REST commits
// response so the API layer can render it directly.
//
// The package is read-only relative to the git repository — it shells
// out to `git cat-file` for object bodies but never modifies the
// repo. Cache writes go through the commit_verification_cache table
// via the sqlc helpers in WriteResult.
package sigverify

import "time"

// Reason mirrors GitHub's documented verification.reason enum on the
// REST commits response. The full set is:
//
//   - valid               — signature checks out + signer email
//     matches one of the user's verified emails.
//   - unsigned            — the commit/tag carries no gpgsig header.
//   - unknown_key         — gpgsig present but the signing subkey
//     fingerprint isn't registered with shithub.
//   - bad_email           — signature checks cryptographically but
//     the signer email doesn't match any of the
//     registered user's emails at all.
//   - unverified_email    — signature checks cryptographically; the
//     email matches a user-emails row but the
//     row's verified flag is false.
//   - malformed_signature — gpgsig header present but the armored
//     block doesn't parse.
//   - invalid             — signature parses, key matches, but the
//     cryptographic check fails.
//   - expired_key         — signature is by a subkey that had already
//     expired by the commit's signature time.
//   - not_signing_key     — signature is by a subkey that exists in
//     the registry but doesn't carry the sign
//     capability flag.
//
// Add new strings here only when GitHub adds them; the value is the
// JSON wire format and is load-bearing for shithub-cli compatibility.
type Reason string

const (
	ReasonValid              Reason = "valid"
	ReasonUnsigned           Reason = "unsigned"
	ReasonUnknownKey         Reason = "unknown_key"
	ReasonBadEmail           Reason = "bad_email"
	ReasonUnverifiedEmail    Reason = "unverified_email"
	ReasonMalformedSignature Reason = "malformed_signature"
	ReasonInvalid            Reason = "invalid"
	ReasonExpiredKey         Reason = "expired_key"
	ReasonNotSigningKey      Reason = "not_signing_key"
)

// Kind discriminates commit-object verifications from annotated-tag
// verifications in the cache table.
type Kind string

const (
	KindCommit Kind = "commit"
	KindTag    Kind = "tag"
)

// Result is the orchestrator's per-object verification verdict. All
// fields except Verified+Reason+VerifiedAt are populated only when
// the corresponding information was available — see field comments.
//
// The shape matches GitHub's verification object on the REST commits
// response (api.go marshalls these directly when assembling the
// `verification` field). Fields that gh exposes as nullable strings
// (`signature`, `payload`) map to empty-string here; the JSON
// encoder layer translates "" back to null at the surface.
type Result struct {
	// Verified is true if and only if Reason == ReasonValid. Stored
	// denormalized so consumers don't have to compare against the
	// string constant.
	Verified bool

	// Reason carries the gh-documented enum value. Always populated.
	Reason Reason

	// Signature is the armored signature block extracted from the
	// gpgsig header. Empty for ReasonUnsigned. Populated for every
	// other reason where a signature was actually present in the
	// object (even ReasonMalformedSignature — we surface the raw
	// armor so the REST consumer can inspect the bad payload).
	Signature string

	// Payload is the canonical commit-object bytes that the signature
	// was computed over (the commit object body with the gpgsig
	// header lines removed). Empty for ReasonUnsigned and when the
	// signature couldn't be parsed.
	Payload []byte

	// SignerUserID is the user-id of the registered GPG key that the
	// signature resolved to. Set whenever Reason ∈ {valid, bad_email,
	// unverified_email, expired_key, not_signing_key, invalid} — any
	// case where the subkey lookup succeeded. Zero when
	// Reason ∈ {unsigned, unknown_key, malformed_signature}.
	SignerUserID int64

	// SignerSubkeyID is the user_gpg_subkeys row id of the signing
	// subkey. Set under the same conditions as SignerUserID.
	SignerSubkeyID int64

	// SignerEmail is the email extracted from the signature's UID
	// embedding (or from the GPG key's primary UID if the signature
	// didn't carry one). Used by the popover UI to render
	// "Signer: <email>".
	SignerEmail string

	// VerifiedAt is the wall-clock time the verification ran.
	VerifiedAt time.Time
}

// unsignedResult is the canonical Result returned when an object has
// no gpgsig header. Constructed once per call so callers always see
// a fresh VerifiedAt; we don't share a package-level value.
func unsignedResult() Result {
	return Result{
		Verified:   false,
		Reason:     ReasonUnsigned,
		VerifiedAt: time.Now(),
	}
}
