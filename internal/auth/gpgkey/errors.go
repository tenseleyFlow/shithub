// SPDX-License-Identifier: AGPL-3.0-or-later

package gpgkey

import (
	"errors"
	"fmt"
)

// MinRSABits is the smallest accepted modulus length for OpenPGP RSA keys.
const MinRSABits = 2048

// MaxKeysPerUser bounds DB rows per user. Mirrors the SSH key cap.
const MaxKeysPerUser = 100

// Sentinel errors. The settings handler surfaces these verbatim as flash
// messages, so each one is a self-contained one-line description from the
// user's perspective.
var (
	// ErrPrivateKeyBlock fires when the uploaded armor is a private key
	// block (BEGIN PGP PRIVATE KEY BLOCK). We never accept private
	// material — the user almost certainly meant to upload the public
	// half.
	ErrPrivateKeyBlock = errors.New("gpgkey: that looks like a private key — please upload your public key (gpg --armor --export <id>)")

	// ErrSignatureBlock fires when the uploaded armor is a detached
	// signature (BEGIN PGP SIGNATURE).
	ErrSignatureBlock = errors.New("gpgkey: that looks like a signature, not a public key")

	// ErrUnparseable covers any other parse failure from the openpgp
	// library. We deliberately don't surface library-internal error
	// messages here; the user typically just needs to know it didn't
	// parse and to try again.
	ErrUnparseable = errors.New("gpgkey: could not parse key — please paste a public key armored block starting with -----BEGIN PGP PUBLIC KEY BLOCK-----")

	// ErrNoIdentities fires for entities with zero UIDs. OpenPGP
	// theoretically allows this but no real-world keyring would produce
	// one; reject so the rest of the pipeline can assume at least one
	// uid.
	ErrNoIdentities = errors.New("gpgkey: key has no user IDs")

	// ErrExpired fires for primary keys that are already expired at
	// upload time. Past commits signed by the key remain verifiable
	// (S52 territory); uploading a brand-new expired key has no
	// consumer.
	ErrExpired = errors.New("gpgkey: this key has expired")

	// ErrUnsupportedAlgo gates DSA and Elgamal-only entities (neither
	// has a sensible signing path on modern git workflows).
	ErrUnsupportedAlgo = errors.New("gpgkey: unsupported key algorithm (accepted: ed25519, ecdsa-nistp256/384/521, RSA ≥ 2048)")

	// ErrRSATooShort fires for RSA primary keys under MinRSABits.
	ErrRSATooShort = fmt.Errorf("gpgkey: RSA keys must be at least %d bits", MinRSABits)

	// ErrMultipleEntities fires when the armor contains more than one
	// entity (key + key, vs key + subkeys-which-are-fine). We accept
	// exactly one primary per upload.
	ErrMultipleEntities = errors.New("gpgkey: please upload one public key at a time")

	// ErrNameTooLong gates the optional user-given title.
	ErrNameTooLong = errors.New("gpgkey: name may be at most 80 characters")

	// ErrNameControl gates control characters in the name field.
	ErrNameControl = errors.New("gpgkey: name contains control characters")
)
