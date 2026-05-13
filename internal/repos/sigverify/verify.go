// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Verify resolves the verification state of a single commit object.
// It reads the commit body via `git cat-file -p`, splits out the
// gpgsig header, looks up the signing subkey via lookups, performs
// the cryptographic check, and finally cross-checks the signer's
// email against the user's verified emails (when applicable).
//
// Never returns ReasonMalformedSignature or ReasonInvalid as errors —
// those are part of the Result. Returns an error only when the
// underlying git or DB call fails (i.e. the verification couldn't be
// attempted at all). Callers should record the verification result
// to the cache table even on error states; the error path is for
// "we don't know yet, retry later" situations.
func Verify(ctx context.Context, gitDir, commitOID string, lookups Lookups) (Result, error) {
	body, err := catFile(ctx, gitDir, commitOID)
	if err != nil {
		return Result{}, fmt.Errorf("sigverify: cat-file %s: %w", commitOID, err)
	}
	return verifyObject(ctx, body, lookups, KindCommit)
}

// VerifyTag is the annotated-tag variant. The commit-object splitter
// already handles both header-form and inline-form signatures, so
// the orchestration is the same — only the cache `kind` discriminator
// differs.
func VerifyTag(ctx context.Context, gitDir, tagOID string, lookups Lookups) (Result, error) {
	body, err := catFile(ctx, gitDir, tagOID)
	if err != nil {
		return Result{}, fmt.Errorf("sigverify: cat-file %s: %w", tagOID, err)
	}
	return verifyObject(ctx, body, lookups, KindTag)
}

// verifyObject is the shared implementation. _ Kind is currently
// unused (the Result doesn't carry it; the caller stamps Kind into
// the cache row themselves), kept on the signature in case future
// branching needs to inspect it.
func verifyObject(ctx context.Context, body []byte, lookups Lookups, _ Kind) (Result, error) {
	payload, armored, signed := splitSignedObject(body)
	if !signed {
		return unsignedResult(), nil
	}

	// Parse the signature packet to learn which subkey signed.
	sigPkt, err := readSignaturePacket(armored)
	if err != nil {
		return Result{
			Verified:   false,
			Reason:     ReasonMalformedSignature,
			Signature:  armored,
			VerifiedAt: time.Now(),
		}, nil
	}

	// Resolve the signing subkey. RFC 4880 supports both an
	// IssuerKeyId subpacket (lower 64 bits of the fingerprint) and
	// an IssuerFingerprint subpacket (the full 40-hex). Modern git/
	// gpg emits both; we prefer the fingerprint when present because
	// 64-bit key ids can collide.
	var fingerprintHex string
	if len(sigPkt.IssuerFingerprint) > 0 {
		fingerprintHex = hex.EncodeToString(sigPkt.IssuerFingerprint)
	}
	if fingerprintHex == "" {
		// IssuerKeyId fallback: we can't do a precise lookup without
		// the full fingerprint. Mark unknown so the user can re-sign
		// with a modern gpg client. (gh produces the same outcome
		// in this rare case.)
		return Result{
			Verified:   false,
			Reason:     ReasonUnknownKey,
			Signature:  armored,
			Payload:    payload,
			VerifiedAt: time.Now(),
		}, nil
	}

	subkey, found, err := lookups.SubkeyByFingerprint(ctx, fingerprintHex)
	if err != nil {
		return Result{}, fmt.Errorf("sigverify: lookup subkey: %w", err)
	}
	if !found {
		return Result{
			Verified:   false,
			Reason:     ReasonUnknownKey,
			Signature:  armored,
			Payload:    payload,
			VerifiedAt: time.Now(),
		}, nil
	}

	// Load the parent gpg-key so we can construct the openpgp.Entity
	// from its armored block. The cryptographic check needs the
	// actual public-key material, not just the fingerprint.
	gpgKey, found, err := lookups.GPGKeyByID(ctx, subkey.GPGKeyID)
	if err != nil {
		return Result{}, fmt.Errorf("sigverify: lookup parent gpg key: %w", err)
	}
	if !found {
		// Parent gone (revoked between subkey lookup and parent
		// lookup). Surface as unknown_key from the user's
		// perspective.
		return Result{
			Verified:   false,
			Reason:     ReasonUnknownKey,
			Signature:  armored,
			Payload:    payload,
			VerifiedAt: time.Now(),
		}, nil
	}

	entity, err := openpgp.ReadArmoredKeyRing(strings.NewReader(gpgKey.Armored))
	if err != nil || len(entity) == 0 {
		// Corrupted at-rest — surface as malformed so the cache row
		// makes the issue visible; rendering UI treats this as
		// unverified.
		return Result{
			Verified:       false,
			Reason:         ReasonMalformedSignature,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			VerifiedAt:     time.Now(),
		}, nil
	}

	// Capability + expiry checks happen BEFORE the cryptographic
	// check. Reason: openpgp.CheckArmoredDetachedSignature does its
	// own expiry check using time.Now() and folds the result into a
	// generic error; running our checks first lets us return the
	// precise gh enum reason (expired_key, not_signing_key).
	if !subkey.CanSign {
		return Result{
			Verified:       false,
			Reason:         ReasonNotSigningKey,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			VerifiedAt:     time.Now(),
		}, nil
	}
	if !subkey.ExpiresAt.IsZero() && sigPkt.CreationTime.After(subkey.ExpiresAt) {
		// Signature was made AFTER the key expired — not valid.
		// Sigs made before expiry remain valid even when the key
		// later expires (gh's behavior).
		return Result{
			Verified:       false,
			Reason:         ReasonExpiredKey,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			VerifiedAt:     time.Now(),
		}, nil
	}

	// Cryptographic check. Pass Config.Time = sig creation time so
	// the openpgp library treats the key as live-at-sig-time (we've
	// already run the explicit expiry check above; the library's
	// re-check would just cause false negatives).
	cfg := &packet.Config{Time: func() time.Time { return sigPkt.CreationTime }}
	signer, err := openpgp.CheckArmoredDetachedSignature(
		entity,
		bytes.NewReader(payload),
		strings.NewReader(armored),
		cfg,
	)
	if err != nil || signer == nil {
		return Result{
			Verified:       false,
			Reason:         ReasonInvalid,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			VerifiedAt:     time.Now(),
		}, nil
	}

	// Email cross-check. Pull the signer email from the signature
	// packet's UID embedding when present, otherwise from the
	// primary identity of the parent gpg key.
	signerEmail := extractSignerEmail(sigPkt, entity[0])

	if signerEmail == "" {
		// No email to cross-check. gh treats this as valid since
		// the cryptography succeeded — we follow suit.
		return Result{
			Verified:       true,
			Reason:         ReasonValid,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			VerifiedAt:     time.Now(),
		}, nil
	}

	emails, err := lookups.UserEmailsByUserID(ctx, gpgKey.UserID)
	if err != nil {
		return Result{}, fmt.Errorf("sigverify: lookup user emails: %w", err)
	}
	emailVerifiedState, claimed := claimEmailLookup(emails, signerEmail)
	switch {
	case !claimed:
		return Result{
			Verified:       false,
			Reason:         ReasonBadEmail,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			SignerEmail:    signerEmail,
			VerifiedAt:     time.Now(),
		}, nil
	case !emailVerifiedState:
		return Result{
			Verified:       false,
			Reason:         ReasonUnverifiedEmail,
			Signature:      armored,
			Payload:        payload,
			SignerUserID:   gpgKey.UserID,
			SignerSubkeyID: subkey.ID,
			SignerEmail:    signerEmail,
			VerifiedAt:     time.Now(),
		}, nil
	}

	return Result{
		Verified:       true,
		Reason:         ReasonValid,
		Signature:      armored,
		Payload:        payload,
		SignerUserID:   gpgKey.UserID,
		SignerSubkeyID: subkey.ID,
		SignerEmail:    signerEmail,
		VerifiedAt:     time.Now(),
	}, nil
}

// catFile shells out to `git cat-file -p <oid>` and returns the
// object body. We trim the trailing newline that git always emits.
func catFile(ctx context.Context, gitDir, oid string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "cat-file", "-p", oid)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readSignaturePacket parses the first signature packet out of an
// armored block. Used to extract the issuer fingerprint + creation
// time + UID embedding without re-doing the full cryptographic check.
func readSignaturePacket(armored string) (*packet.Signature, error) {
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		return nil, err
	}
	if block.Type != "PGP SIGNATURE" {
		return nil, fmt.Errorf("sigverify: expected PGP SIGNATURE block, got %q", block.Type)
	}
	pkt, err := packet.Read(block.Body)
	if err != nil {
		return nil, err
	}
	sig, ok := pkt.(*packet.Signature)
	if !ok {
		return nil, fmt.Errorf("sigverify: first packet is not a Signature")
	}
	return sig, nil
}

// extractSignerEmail returns the email used to sign — preferring the
// signature packet's UID embedding (RFC 4880 §5.2.3.28) when present,
// otherwise the primary UID of the signing entity.
func extractSignerEmail(sig *packet.Signature, e *openpgp.Entity) string {
	if sig.SignerUserId != nil && *sig.SignerUserId != "" {
		// SignerUserId is the full UID string ("Alice <alice@x>");
		// crack out the email part.
		return parseEmailFromUID(*sig.SignerUserId)
	}
	if e != nil {
		if id := e.PrimaryIdentity(); id != nil && id.UserId != nil {
			return id.UserId.Email
		}
	}
	return ""
}

// parseEmailFromUID pulls the email from a UID string of the form
// "Name (Comment) <email@host>" or just "email@host". Falls back to
// the raw string when no angle brackets are present.
func parseEmailFromUID(uid string) string {
	if i := strings.LastIndex(uid, "<"); i >= 0 {
		if j := strings.LastIndex(uid, ">"); j > i {
			return uid[i+1 : j]
		}
	}
	if strings.Contains(uid, "@") {
		return strings.TrimSpace(uid)
	}
	return ""
}

// claimEmailLookup walks the user's emails, returning (verified, true)
// when the email is claimed (case-insensitive match) and (false, false)
// when it isn't claimed at all.
func claimEmailLookup(emails []UserEmail, signerEmail string) (verified, claimed bool) {
	se := strings.ToLower(signerEmail)
	for _, e := range emails {
		if strings.ToLower(e.Email) == se {
			return e.Verified, true
		}
	}
	return false, false
}
