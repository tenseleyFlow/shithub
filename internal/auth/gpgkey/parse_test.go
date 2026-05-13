// SPDX-License-Identifier: AGPL-3.0-or-later

package gpgkey

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// ─── fixture helpers ────────────────────────────────────────────────
//
// We synthesize all test fixtures in-memory via ProtonMail/go-crypto
// rather than shipping committed .asc files. Tests then exercise the
// real codec end-to-end (serialize → parse → assert) without depending
// on a system gpg binary.

// newEd25519 returns a freshly-generated ed25519 entity with a single
// UID. ProtonMail's nil-config default is RSA-2048; we have to ask
// for EdDSA explicitly.
func newEd25519(t *testing.T, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("shithub-test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	if err != nil {
		t.Fatalf("NewEntity ed25519: %v", err)
	}
	return e
}

// newRSA returns an RSA entity at the requested bit size.
func newRSA(t *testing.T, email string, bits int) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("shithub-test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   bits,
	})
	if err != nil {
		t.Fatalf("NewEntity rsa%d: %v", bits, err)
	}
	return e
}

// armoredPublic serializes an entity's public-key block as ASCII armor.
func armoredPublic(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("entity.Serialize: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
	return buf.String()
}

// armoredPrivate serializes the SECRET key block — the "user uploaded
// their private key by mistake" fixture.
func armoredPrivate(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PRIVATE KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode private: %v", err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatalf("entity.SerializePrivate: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
	return buf.String()
}

// armoredDetachedSig returns an armored detached signature over a
// small payload — the "user uploaded a signature by mistake" fixture.
func armoredDetachedSig(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP SIGNATURE", nil)
	if err != nil {
		t.Fatalf("armor.Encode sig: %v", err)
	}
	if err := openpgp.DetachSign(w, e, strings.NewReader("hello"), nil); err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
	return buf.String()
}

// ─── happy-path tests ───────────────────────────────────────────────

func TestParse_Ed25519(t *testing.T) {
	e := newEd25519(t, "alice@shithub.test")
	armored := armoredPublic(t, e)

	got, err := Parse("My laptop", armored)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got.Name != "My laptop" {
		t.Errorf("Name: got %q, want %q", got.Name, "My laptop")
	}
	if len(got.Fingerprint) != 40 {
		t.Errorf("Fingerprint length: got %d, want 40", len(got.Fingerprint))
	}
	if !isHex(got.Fingerprint) {
		t.Errorf("Fingerprint not hex: %q", got.Fingerprint)
	}
	if len(got.KeyID) != 16 || !isHex(got.KeyID) {
		t.Errorf("KeyID malformed: %q", got.KeyID)
	}
	if !strings.HasSuffix(got.Fingerprint, got.KeyID) {
		t.Errorf("KeyID should be lower 16 of fingerprint: fp=%s key_id=%s", got.Fingerprint, got.KeyID)
	}
	if got.PrimaryAlgo != "ed25519" {
		t.Errorf("PrimaryAlgo: got %q, want ed25519", got.PrimaryAlgo)
	}
	if !got.CanSign {
		t.Error("expected CanSign=true for default ed25519 primary")
	}
	if !got.CanCertify {
		t.Error("expected CanCertify=true for default ed25519 primary")
	}
	if got.ExpiresAt != nil {
		t.Errorf("expected ExpiresAt=nil for default no-expiry key; got %v", got.ExpiresAt)
	}
	if len(got.UIDs) != 1 || got.UIDs[0] != "alice@shithub.test" {
		t.Errorf("UIDs: got %v, want [alice@shithub.test]", got.UIDs)
	}
	// Default openpgp.NewEntity creates one encryption subkey.
	if len(got.Subkeys) < 1 {
		t.Errorf("expected at least one subkey; got %d", len(got.Subkeys))
	}
}

func TestParse_RSA4096(t *testing.T) {
	e := newRSA(t, "bob@shithub.test", 4096)
	armored := armoredPublic(t, e)
	got, err := Parse("", armored)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.PrimaryAlgo != "rsa4096" {
		t.Errorf("PrimaryAlgo: got %q, want rsa4096", got.PrimaryAlgo)
	}
	if !got.CanSign || !got.CanCertify {
		t.Errorf("expected sign+certify on RSA primary; got sign=%t certify=%t", got.CanSign, got.CanCertify)
	}
}

func TestParse_EncryptOnly_Accepted(t *testing.T) {
	// Build an entity, then strip its primary's sign+certify flags so
	// it's encrypt-only. Re-issue the self-signature so the modified
	// flags persist through Serialize.
	e := newRSA(t, "encryptonly@shithub.test", 2048)
	for _, id := range e.Identities {
		id.SelfSignature.FlagSign = false
		id.SelfSignature.FlagCertify = false
		id.SelfSignature.FlagEncryptCommunications = true
		id.SelfSignature.FlagEncryptStorage = true
		// Re-sign with the modified flags.
		if err := id.SelfSignature.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey, nil); err != nil {
			t.Fatalf("re-sign identity: %v", err)
		}
	}
	armored := armoredPublic(t, e)

	got, err := Parse("encryption only key", armored)
	if err != nil {
		t.Fatalf("Parse should accept encryption-only keys (gh parity); got: %v", err)
	}
	if got.CanSign {
		t.Error("CanSign: got true, want false on encryption-only primary")
	}
	if !got.CanEncryptComms && !got.CanEncryptStorage {
		t.Error("expected at least one encrypt-* flag true")
	}
}

func TestParse_MultiSubkey(t *testing.T) {
	e := newEd25519(t, "multi@shithub.test")
	// Add an extra signing subkey. ProtonMail/go-crypto's AddSigningSubkey
	// requires a Config to specify the algorithm.
	if err := e.AddSigningSubkey(nil); err != nil {
		t.Fatalf("AddSigningSubkey: %v", err)
	}
	armored := armoredPublic(t, e)
	got, err := Parse("", armored)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Subkeys) < 2 {
		t.Errorf("expected >=2 subkeys (one encryption from default, one we added); got %d", len(got.Subkeys))
	}
	// At least one subkey should have can_sign.
	anySigning := false
	for _, sk := range got.Subkeys {
		if sk.CanSign {
			anySigning = true
			break
		}
	}
	if !anySigning {
		t.Error("expected at least one signing subkey")
	}
}

// ─── rejection tests ────────────────────────────────────────────────

func TestParse_PrivateKeyBlock(t *testing.T) {
	e := newEd25519(t, "private@shithub.test")
	armored := armoredPrivate(t, e)
	_, err := Parse("", armored)
	if !errors.Is(err, ErrPrivateKeyBlock) {
		t.Errorf("err: got %v, want ErrPrivateKeyBlock", err)
	}
}

func TestParse_SignatureBlock(t *testing.T) {
	e := newEd25519(t, "sig@shithub.test")
	armored := armoredDetachedSig(t, e)
	_, err := Parse("", armored)
	if !errors.Is(err, ErrSignatureBlock) {
		t.Errorf("err: got %v, want ErrSignatureBlock", err)
	}
}

func TestParse_Expired(t *testing.T) {
	// Create an entity with a backdated creation time + a short lifetime
	// so the key is already expired by `time.Now()`.
	past := time.Now().Add(-48 * time.Hour)
	cfg := &packet.Config{
		Time: func() time.Time { return past },
	}
	e, err := openpgp.NewEntity("shithub-expired", "", "expired@shithub.test", cfg)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	// 1-hour lifetime from "past" → expired ~47 hours ago.
	oneHour := uint32(3600)
	for _, id := range e.Identities {
		id.SelfSignature.KeyLifetimeSecs = &oneHour
		if err := id.SelfSignature.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey, cfg); err != nil {
			t.Fatalf("re-sign for expiry: %v", err)
		}
	}
	armored := armoredPublic(t, e)
	_, err = Parse("", armored)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("err: got %v, want ErrExpired", err)
	}
}

func TestParse_RSATooShort(t *testing.T) {
	e := newRSA(t, "short@shithub.test", 1024)
	armored := armoredPublic(t, e)
	_, err := Parse("", armored)
	if !errors.Is(err, ErrRSATooShort) {
		t.Errorf("err: got %v, want ErrRSATooShort", err)
	}
}

func TestParse_Garbage(t *testing.T) {
	_, err := Parse("", "not a key at all, just garbage")
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err: got %v, want ErrUnparseable", err)
	}
}

func TestParse_Empty(t *testing.T) {
	_, err := Parse("", "")
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err: got %v, want ErrUnparseable", err)
	}
}

func TestParse_LeadingWhitespaceTolerated(t *testing.T) {
	e := newEd25519(t, "ws@shithub.test")
	armored := "\n\n  \t" + armoredPublic(t, e)
	if _, err := Parse("", armored); err != nil {
		t.Errorf("Parse should trim leading whitespace; got %v", err)
	}
}

// ─── name-validation tests ──────────────────────────────────────────

func TestParse_NameTooLong(t *testing.T) {
	e := newEd25519(t, "n@shithub.test")
	armored := armoredPublic(t, e)
	long := strings.Repeat("x", 81)
	_, err := Parse(long, armored)
	if !errors.Is(err, ErrNameTooLong) {
		t.Errorf("err: got %v, want ErrNameTooLong", err)
	}
}

func TestParse_NameControlChars(t *testing.T) {
	e := newEd25519(t, "n@shithub.test")
	armored := armoredPublic(t, e)
	_, err := Parse("bad\x00name", armored)
	if !errors.Is(err, ErrNameControl) {
		t.Errorf("err: got %v, want ErrNameControl", err)
	}
}

// ─── regression baseline: real gpg-produced fixtures ──────────────
//
// Exercises codec compatibility with output from `gpg (GnuPG)` 2.5+.
// See testdata/README.md for the generation recipe.

func TestParse_RealGPGFixtures(t *testing.T) {
	cases := []struct {
		name string
		path string
		algo string
	}{
		{"ed25519", "testdata/ed25519.asc", "ed25519"},
		{"rsa4096", "testdata/rsa4096.asc", "rsa4096"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			blob, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("ReadFile %s: %v", tc.path, err)
			}
			got, err := Parse("", string(blob))
			if err != nil {
				t.Fatalf("Parse %s: %v", tc.path, err)
			}
			if got.PrimaryAlgo != tc.algo {
				t.Errorf("PrimaryAlgo: got %q, want %q", got.PrimaryAlgo, tc.algo)
			}
			if !got.CanSign && !got.CanCertify {
				t.Errorf("expected sign or certify on real gpg primary; got both false")
			}
			if len(got.UIDs) == 0 {
				t.Error("expected at least one UID")
			}
			if len(got.Fingerprint) != 40 || !isHex(got.Fingerprint) {
				t.Errorf("Fingerprint malformed: %q", got.Fingerprint)
			}
		})
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
