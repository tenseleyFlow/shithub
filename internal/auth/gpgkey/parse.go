// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gpgkey wraps OpenPGP public-key parsing, validation, and
// fingerprinting. Every settings handler, REST endpoint, and future
// import path that accepts user-supplied PGP keys goes through Parse
// so the algorithm whitelist + capability extraction lives in exactly
// one place.
//
// shithub mirrors GitHub's /user/gpg_keys response shape; the Parsed
// type carries all the fields that response needs so callers don't
// re-parse the armored block on read. Verification (S51 sub-PR 2) and
// rendering (S51 sub-PR 5) consume the same Parsed type via the sqlc
// row mapping.
package gpgkey

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Armor block type constants per RFC 4880 §6.2.
const (
	armorTypePublicKey  = "PGP PUBLIC KEY BLOCK"
	armorTypePrivateKey = "PGP PRIVATE KEY BLOCK"
	armorTypeSignature  = "PGP SIGNATURE"
)

// Parsed is the validated, ready-to-store representation of a user-
// supplied PGP public key. Mirrors the gh /user/gpg_keys response
// shape; the sqlc Insert path consumes this struct directly.
type Parsed struct {
	// Name is the optional user-given title (gh's "name" field).
	// Blank string when the user omitted it.
	Name string

	// Fingerprint is the canonical lowercase 40-hex SHA-1 fingerprint
	// of the primary public key packet.
	Fingerprint string

	// KeyID is the lower 64 bits of the fingerprint, lowercase hex
	// (16 chars). Stored denormalized for log-line lookups.
	KeyID string

	// Armored is the ASCII-armored block exactly as uploaded, round-
	// trippable. Stored verbatim so the REST `public_key` / `raw_key`
	// fields can be served without re-armoring.
	Armored string

	// Primary-key capability flags decoded from the primary identity's
	// self-signature. Split per RFC 4880 §5.2.3.21 to match gh's
	// can_encrypt_comms / can_encrypt_storage shape.
	CanSign           bool
	CanEncryptComms   bool
	CanEncryptStorage bool
	CanCertify        bool
	CanAuthenticate   bool

	// UIDs are the email addresses parsed from the entity's
	// identities. May be empty strings for identities without an email
	// component (gh tolerates these; we surface them as "" entries).
	UIDs []string

	// Subkeys is the per-subkey metadata used both for the user_gpg_
	// subkeys table inserts and for the REST nested-subkeys response
	// shape.
	Subkeys []ParsedSubkey

	// PrimaryAlgo is a short ASCII description like "ed25519" or
	// "rsa4096". For UI display only.
	PrimaryAlgo string

	// ExpiresAt is the primary key's expiration timestamp, nil for
	// keys that never expire.
	ExpiresAt *time.Time
}

// ParsedSubkey carries per-subkey metadata for the user_gpg_subkeys
// table + REST response.
type ParsedSubkey struct {
	Fingerprint       string
	KeyID             string
	CanSign           bool
	CanEncryptComms   bool
	CanEncryptStorage bool
	CanCertify        bool
	ExpiresAt         *time.Time
}

// Parse validates a user-supplied armored OpenPGP public-key block.
// Returns ErrPrivateKeyBlock / ErrSignatureBlock when the user pasted
// the wrong block type; ErrUnparseable for any other parse failure;
// the algorithm / expiry / no-uids errors as appropriate; or *Parsed
// on success.
//
// Encryption-only keys (no signing capability on the primary or any
// subkey) are ACCEPTED — gh parity. Surface can_sign=false in the
// REST response; clients can filter on the flag.
func Parse(name, armored string) (*Parsed, error) {
	trimmedName := strings.TrimSpace(name)
	if len(trimmedName) > 80 {
		return nil, ErrNameTooLong
	}
	if hasControlChars(trimmedName) {
		return nil, ErrNameControl
	}

	// Peek at the armor block type so we can produce a precise error
	// for the private-key / signature mistakes (the most common user
	// errors). openpgp.ReadArmoredKeyRing rejects both with a generic
	// "no public keys found" error otherwise.
	armored = strings.TrimLeft(armored, "\r\n\t ")
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		return nil, ErrUnparseable
	}
	switch block.Type {
	case armorTypePublicKey:
		// OK; parse as a key block.
	case armorTypePrivateKey:
		return nil, ErrPrivateKeyBlock
	case armorTypeSignature:
		return nil, ErrSignatureBlock
	default:
		return nil, ErrUnparseable
	}

	// We've consumed the armor reader above. Reparse from the original
	// string via ReadArmoredKeyRing so we get a populated EntityList
	// without re-implementing the packet walker.
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil || len(entities) == 0 {
		return nil, ErrUnparseable
	}
	if len(entities) > 1 {
		return nil, ErrMultipleEntities
	}
	e := entities[0]

	if e.PrimaryKey == nil {
		return nil, ErrUnparseable
	}
	if len(e.Identities) == 0 {
		return nil, ErrNoIdentities
	}

	primaryAlgo, ok := algoLabel(e.PrimaryKey)
	if !ok {
		return nil, ErrUnsupportedAlgo
	}
	if e.PrimaryKey.PubKeyAlgo == packet.PubKeyAlgoRSA ||
		e.PrimaryKey.PubKeyAlgo == packet.PubKeyAlgoRSASignOnly ||
		e.PrimaryKey.PubKeyAlgo == packet.PubKeyAlgoRSAEncryptOnly {
		bits, err := e.PrimaryKey.BitLength()
		if err != nil {
			return nil, ErrUnparseable
		}
		if int(bits) < MinRSABits {
			return nil, ErrRSATooShort
		}
	}

	primaryID := e.PrimaryIdentity()
	if primaryID == nil || primaryID.SelfSignature == nil {
		return nil, ErrUnparseable
	}

	// Expiry: a primary's KeyLifetimeSecs lives on the primary
	// identity's self-signature. nil => never expires.
	primaryExpires := keyExpiry(e.PrimaryKey.CreationTime, primaryID.SelfSignature.KeyLifetimeSecs)
	if primaryExpires != nil && primaryExpires.Before(time.Now()) {
		return nil, ErrExpired
	}

	// Capability flags decoded from the primary self-sig. When the
	// flags subpacket is absent (some very old keys), the boolean
	// fields on the signature default to false — gh interprets the
	// same way so we don't need to special-case.
	canSign, canEncComms, canEncStorage, canCertify, canAuth := capabilityFlags(primaryID.SelfSignature)

	parsed := &Parsed{
		Name:              trimmedName,
		Fingerprint:       hex.EncodeToString(e.PrimaryKey.Fingerprint),
		KeyID:             fmt.Sprintf("%016x", e.PrimaryKey.KeyId),
		Armored:           strings.TrimRight(armored, "\r\n\t ") + "\n",
		CanSign:           canSign,
		CanEncryptComms:   canEncComms,
		CanEncryptStorage: canEncStorage,
		CanCertify:        canCertify,
		CanAuthenticate:   canAuth,
		PrimaryAlgo:       primaryAlgo,
		ExpiresAt:         primaryExpires,
	}

	for uidKey := range e.Identities {
		email := e.Identities[uidKey].UserId.Email
		parsed.UIDs = append(parsed.UIDs, email)
	}
	if parsed.UIDs == nil {
		parsed.UIDs = []string{}
	}

	for i := range e.Subkeys {
		sk := &e.Subkeys[i]
		if sk.PublicKey == nil || sk.Sig == nil {
			continue
		}
		skCanSign, skCanEncComms, skCanEncStorage, skCanCertify, _ := capabilityFlags(sk.Sig)
		parsed.Subkeys = append(parsed.Subkeys, ParsedSubkey{
			Fingerprint:       hex.EncodeToString(sk.PublicKey.Fingerprint),
			KeyID:             fmt.Sprintf("%016x", sk.PublicKey.KeyId),
			CanSign:           skCanSign,
			CanEncryptComms:   skCanEncComms,
			CanEncryptStorage: skCanEncStorage,
			CanCertify:        skCanCertify,
			ExpiresAt:         keyExpiry(sk.PublicKey.CreationTime, sk.Sig.KeyLifetimeSecs),
		})
	}
	if parsed.Subkeys == nil {
		parsed.Subkeys = []ParsedSubkey{}
	}

	return parsed, nil
}

// algoLabel returns a short UI-friendly label for the key algorithm.
// Returns (label, true) when the algorithm is accepted; ("", false)
// to reject (DSA, Elgamal-only).
func algoLabel(pk *packet.PublicKey) (string, bool) {
	switch pk.PubKeyAlgo {
	case packet.PubKeyAlgoRSA, packet.PubKeyAlgoRSASignOnly, packet.PubKeyAlgoRSAEncryptOnly:
		bits, _ := pk.BitLength()
		return fmt.Sprintf("rsa%d", bits), true
	case packet.PubKeyAlgoEdDSA:
		return "ed25519", true
	case packet.PubKeyAlgoECDSA:
		return "ecdsa", true
	case packet.PubKeyAlgoECDH:
		// Encryption-capable elliptic. Accept; surface honestly.
		return "ecdh", true
	case packet.PubKeyAlgoDSA:
		return "", false
	case packet.PubKeyAlgoElGamal:
		return "", false
	}
	return "", false
}

// capabilityFlags decodes the can_sign / can_encrypt_* / can_certify /
// can_authenticate flags from a self-signature or subkey-binding
// signature. The ProtonMail/go-crypto package surfaces these as
// individual booleans on the Signature struct.
func capabilityFlags(sig *packet.Signature) (canSign, canEncComms, canEncStorage, canCertify, canAuth bool) {
	if sig == nil {
		return
	}
	// When FlagsValid is false the flag subpacket was absent. RFC 4880
	// then says implementations should infer capabilities from the key
	// algorithm; we follow gh's behavior of treating absent flags as
	// "no explicit capabilities asserted" and surfacing all false.
	if !sig.FlagsValid {
		return
	}
	return sig.FlagSign, sig.FlagEncryptCommunications, sig.FlagEncryptStorage, sig.FlagCertify, sig.FlagAuthenticate
}

// keyExpiry computes an absolute expiration time from a creation time
// plus an optional lifetime-in-seconds (the self-sig subpacket).
// Returns nil for keys that never expire.
func keyExpiry(creation time.Time, lifetimeSecs *uint32) *time.Time {
	if lifetimeSecs == nil || *lifetimeSecs == 0 {
		return nil
	}
	t := creation.Add(time.Duration(*lifetimeSecs) * time.Second)
	return &t
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
