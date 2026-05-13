// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"context"
	"time"
)

// Lookups is the interface the orchestrator uses to resolve
// signature artifacts (subkeys, parent gpg keys, user emails) back
// to user records. The interface exists so tests can pass in a
// fake without touching sqlc; production wires through to
// usersdb.Queries.
type Lookups interface {
	// SubkeyByFingerprint looks up a user_gpg_subkeys row by its
	// 40-hex canonical fingerprint. Returns (subkey, true, nil) on
	// hit, (_, false, nil) on miss, and (_, _, err) only on DB
	// errors (miss is NOT an error — the orchestrator translates
	// it into ReasonUnknownKey).
	SubkeyByFingerprint(ctx context.Context, fingerprint string) (Subkey, bool, error)

	// GPGKeyByID resolves a primary gpg-key row by id. Called once
	// per Verify invocation after SubkeyByFingerprint returns a hit
	// so the orchestrator has access to the primary's armored block
	// (needed to construct the openpgp.Entity for cryptographic
	// verification).
	GPGKeyByID(ctx context.Context, id int64) (GPGKey, bool, error)

	// UserEmailsByUserID returns every email row for the given user
	// (verified or not). The orchestrator uses this for the
	// `bad_email` vs `unverified_email` discrimination — gh's
	// verification check fails open as bad_email if the signature's
	// email isn't claimed by the user at all, and as
	// unverified_email if the email is claimed but unverified.
	UserEmailsByUserID(ctx context.Context, userID int64) ([]UserEmail, error)
}

// Subkey is the orchestrator-facing shape of a user_gpg_subkeys row.
// Decoupled from sqlc.UserGpgSubkey so tests don't have to construct
// pgtype.Timestamptz values.
type Subkey struct {
	ID                int64
	GPGKeyID          int64
	Fingerprint       string
	KeyID             string
	CanSign           bool
	CanEncryptComms   bool
	CanEncryptStorage bool
	CanCertify        bool
	// ExpiresAt is the subkey's expiration timestamp; the zero Time
	// value means "never expires".
	ExpiresAt time.Time
	// RevokedAt is set if this subkey has been soft-deleted. The
	// orchestrator treats revoked subkeys as if they don't exist
	// (returns ReasonUnknownKey), so production lookups should
	// already filter `revoked_at is null` and this field is mostly
	// here for diagnostic completeness.
	RevokedAt time.Time
}

// GPGKey is the orchestrator-facing shape of a user_gpg_keys row.
type GPGKey struct {
	ID          int64
	UserID      int64
	Fingerprint string
	KeyID       string
	// Armored is the full ASCII-armored block. The orchestrator
	// re-parses this via ProtonMail/go-crypto to access the actual
	// public-key material for cryptographic verification.
	Armored string
}

// UserEmail is the orchestrator-facing shape of a user_emails row.
type UserEmail struct {
	Email    string
	Verified bool
}
