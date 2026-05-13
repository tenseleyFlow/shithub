// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// ─── fixture helpers ────────────────────────────────────────────────

// newTestRepo creates a fresh bare git repo in a t.TempDir and returns
// its gitDir.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", "--bare", dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	return dir
}

// hashObject pipes commitBody through `git hash-object -w -t commit
// --stdin` and returns the resulting OID. The body is stored as a
// commit object in the bare repo; we don't update any ref (the
// orchestrator addresses objects by OID directly, no ref needed).
func hashObject(t *testing.T, gitDir string, commitBody []byte) string {
	t.Helper()
	cmd := exec.Command("git", "-C", gitDir, "hash-object", "-w", "-t", "commit", "--stdin")
	cmd.Stdin = bytes.NewReader(commitBody)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// signedCommit constructs a git commit object body signed by entity.
// The tree OID is the well-known empty-tree OID; we don't care about
// tree validity since verification only reads the commit object body.
//
// signedAt sets the signature creation time (useful for the expiry
// test which needs a sig made before the key's expiry timestamp).
func signedCommit(t *testing.T, entity *openpgp.Entity, message string, signedAt time.Time) []byte {
	t.Helper()
	const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	authorTime := signedAt.Unix()
	body := fmt.Sprintf(
		"tree %s\nauthor Alice <alice@shithub.test> %d +0000\ncommitter Alice <alice@shithub.test> %d +0000\n\n%s\n",
		emptyTree, authorTime, authorTime, message,
	)

	var sigBuf bytes.Buffer
	armorWriter, err := armor.Encode(&sigBuf, "PGP SIGNATURE", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	cfg := &packet.Config{Time: func() time.Time { return signedAt }}
	if err := openpgp.DetachSign(armorWriter, entity, strings.NewReader(body), cfg); err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	if err := armorWriter.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}

	// Reformat the signature block as a gpgsig header (each line
	// after the first prefixed with a single space).
	sigStr := strings.TrimRight(sigBuf.String(), "\n")
	sigLines := strings.Split(sigStr, "\n")
	indented := []string{"gpgsig " + sigLines[0]}
	for _, line := range sigLines[1:] {
		indented = append(indented, " "+line)
	}
	gpgsigHeader := strings.Join(indented, "\n")

	// Insert the gpgsig header between the committer line and the
	// blank-line + message.
	signedBody := strings.Replace(
		body,
		fmt.Sprintf("committer Alice <alice@shithub.test> %d +0000\n\n", authorTime),
		fmt.Sprintf("committer Alice <alice@shithub.test> %d +0000\n%s\n\n", authorTime, gpgsigHeader),
		1,
	)
	return []byte(signedBody)
}

// unsignedCommit builds a commit object body with no gpgsig header.
func unsignedCommit(t *testing.T, message string) []byte {
	t.Helper()
	const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	now := time.Now().Unix()
	return []byte(fmt.Sprintf(
		"tree %s\nauthor Alice <alice@shithub.test> %d +0000\ncommitter Alice <alice@shithub.test> %d +0000\n\n%s\n",
		emptyTree, now, now, message,
	))
}

// armoredPublic returns the entity's armored public-key block.
func armoredPublic(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	w.Close()
	return buf.String()
}

// signingSubkey returns the first subkey of an entity that carries
// the sign flag, falling back to the primary key if no subkey is
// signing-capable.
func signingSubkey(t *testing.T, e *openpgp.Entity) *packet.PublicKey {
	t.Helper()
	for i := range e.Subkeys {
		sk := &e.Subkeys[i]
		if sk.Sig != nil && sk.Sig.FlagSign {
			return sk.PublicKey
		}
	}
	return e.PrimaryKey
}

// fakeLookups is a minimal Lookups implementation for tests. The
// orchestrator only calls SubkeyByFingerprint / GPGKeyByID /
// UserEmailsByUserID, and we set each return value explicitly per
// test.
type fakeLookups struct {
	subkey      Subkey
	subkeyFound bool
	gpgKey      GPGKey
	gpgKeyFound bool
	emails      []UserEmail
	emailsErr   error
	subkeyErr   error
	gpgKeyErr   error
}

func (f *fakeLookups) SubkeyByFingerprint(_ context.Context, fp string) (Subkey, bool, error) {
	if f.subkeyErr != nil {
		return Subkey{}, false, f.subkeyErr
	}
	if !f.subkeyFound {
		return Subkey{}, false, nil
	}
	if !strings.EqualFold(fp, f.subkey.Fingerprint) {
		return Subkey{}, false, nil
	}
	return f.subkey, true, nil
}

func (f *fakeLookups) GPGKeyByID(_ context.Context, id int64) (GPGKey, bool, error) {
	if f.gpgKeyErr != nil {
		return GPGKey{}, false, f.gpgKeyErr
	}
	if !f.gpgKeyFound {
		return GPGKey{}, false, nil
	}
	if f.gpgKey.ID != id {
		return GPGKey{}, false, nil
	}
	return f.gpgKey, true, nil
}

func (f *fakeLookups) UserEmailsByUserID(_ context.Context, _ int64) ([]UserEmail, error) {
	return f.emails, f.emailsErr
}

// lookupsForEntity wires the in-memory entity into a fakeLookups so
// that SubkeyByFingerprint and GPGKeyByID return matching records.
// signingFP is the fingerprint of the subkey that was used to sign;
// some tests want to verify against a non-signing subkey (the
// not_signing_key case), so the caller supplies which subkey.
func lookupsForEntity(t *testing.T, entity *openpgp.Entity, signerPK *packet.PublicKey, canSign bool, expiresAt time.Time, emails []UserEmail) *fakeLookups {
	t.Helper()
	return &fakeLookups{
		subkey: Subkey{
			ID:          42,
			GPGKeyID:    99,
			Fingerprint: hex.EncodeToString(signerPK.Fingerprint),
			KeyID:       fmt.Sprintf("%016x", signerPK.KeyId),
			CanSign:     canSign,
			ExpiresAt:   expiresAt,
		},
		subkeyFound: true,
		gpgKey: GPGKey{
			ID:          99,
			UserID:      7,
			Fingerprint: hex.EncodeToString(entity.PrimaryKey.Fingerprint),
			KeyID:       fmt.Sprintf("%016x", entity.PrimaryKey.KeyId),
			Armored:     armoredPublic(t, entity),
		},
		gpgKeyFound: true,
		emails:      emails,
	}
}

// ─── tests ──────────────────────────────────────────────────────────

// makeEntity builds an ed25519 entity for tests. ProtonMail's default
// is RSA-2048; we ask for EdDSA explicitly.
func makeEntity(t *testing.T, email string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("shithub-test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

func TestVerify_Unsigned(t *testing.T) {
	gitDir := newTestRepo(t)
	oid := hashObject(t, gitDir, unsignedCommit(t, "hello world"))

	got, err := Verify(context.Background(), gitDir, oid, &fakeLookups{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonUnsigned {
		t.Errorf("reason: got %q, want unsigned", got.Reason)
	}
	if got.Verified {
		t.Error("expected Verified=false on unsigned commit")
	}
}

func TestVerify_Valid(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	body := signedCommit(t, entity, "valid sig", time.Now())
	oid := hashObject(t, gitDir, body)

	signer := signingSubkey(t, entity)
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, true, time.Time{}, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonValid {
		t.Fatalf("reason: got %q (sig=%q), want valid", got.Reason, got.Signature)
	}
	if !got.Verified {
		t.Error("expected Verified=true")
	}
	if got.SignerUserID != 7 {
		t.Errorf("SignerUserID: got %d, want 7", got.SignerUserID)
	}
	if got.SignerEmail != "alice@shithub.test" {
		t.Errorf("SignerEmail: got %q, want alice@shithub.test", got.SignerEmail)
	}
}

func TestVerify_UnknownKey(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	body := signedCommit(t, entity, "unknown key sig", time.Now())
	oid := hashObject(t, gitDir, body)

	// Lookups returns no subkey for any fingerprint.
	got, err := Verify(context.Background(), gitDir, oid, &fakeLookups{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonUnknownKey {
		t.Errorf("reason: got %q, want unknown_key", got.Reason)
	}
}

func TestVerify_BadEmail(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	body := signedCommit(t, entity, "bad email sig", time.Now())
	oid := hashObject(t, gitDir, body)

	signer := signingSubkey(t, entity)
	// User registered some other email, never claimed alice@shithub.test.
	emails := []UserEmail{{Email: "bob@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, true, time.Time{}, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonBadEmail {
		t.Errorf("reason: got %q, want bad_email", got.Reason)
	}
}

func TestVerify_UnverifiedEmail(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	body := signedCommit(t, entity, "unverified email sig", time.Now())
	oid := hashObject(t, gitDir, body)

	signer := signingSubkey(t, entity)
	// Email claimed but unverified.
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: false}}
	lookups := lookupsForEntity(t, entity, signer, true, time.Time{}, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonUnverifiedEmail {
		t.Errorf("reason: got %q, want unverified_email", got.Reason)
	}
}

func TestVerify_NotSigningKey(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	body := signedCommit(t, entity, "not signing key", time.Now())
	oid := hashObject(t, gitDir, body)

	signer := signingSubkey(t, entity)
	// Look up the same fingerprint but with CanSign=false.
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, false /* canSign */, time.Time{}, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonNotSigningKey {
		t.Errorf("reason: got %q, want not_signing_key", got.Reason)
	}
}

func TestVerify_ExpiredKey(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	// Sign with a creation time of "now"; pretend the subkey expired
	// an hour ago.
	signedAt := time.Now()
	body := signedCommit(t, entity, "expired key sig", signedAt)
	oid := hashObject(t, gitDir, body)

	signer := signingSubkey(t, entity)
	expired := signedAt.Add(-time.Hour)
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, true, expired, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonExpiredKey {
		t.Errorf("reason: got %q, want expired_key", got.Reason)
	}
}

func TestVerify_Invalid(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)

	// Build commit A (the one we'll write) and capture its gpgsig
	// header. Build commit B with a DIFFERENT message; capture its
	// gpgsig header. Then write commit A's body but with B's
	// signature embedded — the armor still parses cleanly (no
	// malformed_signature), but the cryptographic check fails
	// because the signature was made over commit B's payload, not
	// commit A's. This is the canonical "invalid" branch.
	bodyA := signedCommit(t, entity, "commit A message", time.Now())
	bodyB := signedCommit(t, entity, "commit B different message", time.Now())

	sigA := extractGpgsig(t, bodyA)
	sigB := extractGpgsig(t, bodyB)

	mismatched := bytes.Replace(bodyA, sigA, sigB, 1)
	oid := hashObject(t, gitDir, mismatched)

	signer := signingSubkey(t, entity)
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, true, time.Time{}, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonInvalid {
		t.Errorf("reason: got %q, want invalid", got.Reason)
	}
}

// extractGpgsig returns the full gpgsig header block (header label +
// continuation lines, trailing newline) from a commit body.
func extractGpgsig(t *testing.T, body []byte) []byte {
	t.Helper()
	idx := bytes.Index(body, []byte("\ngpgsig "))
	if idx < 0 {
		t.Fatalf("commit body has no gpgsig header")
	}
	start := idx + 1 // skip the preceding newline
	// Walk to the first non-continuation line (no leading space).
	cursor := start
	for cursor < len(body) {
		nl := bytes.IndexByte(body[cursor:], '\n')
		if nl < 0 {
			break
		}
		lineEnd := cursor + nl + 1
		// Peek at the next line's first byte; if it's not a
		// continuation (" "), the header block ends here.
		if lineEnd >= len(body) || body[lineEnd] != ' ' {
			return body[start:lineEnd]
		}
		cursor = lineEnd
	}
	return body[start:]
}

func TestVerify_MalformedSignature(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)
	body := signedCommit(t, entity, "malformed", time.Now())

	// Replace "-----BEGIN PGP SIGNATURE-----" with "-----BEGIN PGP
	// XYZSIGNATURE-----" — the armor parser will refuse the unknown
	// block type.
	malformed := bytes.Replace(body, []byte("BEGIN PGP SIGNATURE"), []byte("BEGIN PGP XYZSIGNATURE"), 1)
	malformed = bytes.Replace(malformed, []byte("END PGP SIGNATURE"), []byte("END PGP XYZSIGNATURE"), 1)
	oid := hashObject(t, gitDir, malformed)

	signer := signingSubkey(t, entity)
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, true, time.Time{}, emails)

	got, err := Verify(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Reason != ReasonMalformedSignature {
		t.Errorf("reason: got %q, want malformed_signature", got.Reason)
	}
}

// TestVerify_GitCatFileError exercises the error-return path
// (gitDir unreadable / OID nonexistent → orchestrator returns an
// error rather than a Result).
func TestVerify_GitCatFileError(t *testing.T) {
	gitDir := newTestRepo(t)
	_, err := Verify(context.Background(), gitDir, strings.Repeat("0", 40), &fakeLookups{})
	if err == nil {
		t.Error("expected error for nonexistent OID; got nil")
	}
}

// TestVerifyTag walks the same valid-sig path against a tag object.
// We construct the tag object body with the inline trailing-signature
// form (legacy git tag -s convention).
func TestVerifyTag_ValidInline(t *testing.T) {
	entity := makeEntity(t, "alice@shithub.test")
	gitDir := newTestRepo(t)

	// Build the tag object payload (everything except the signature)
	// and sign it.
	const emptyCommit = "0000000000000000000000000000000000000000"
	tagPayload := fmt.Sprintf(
		"object %s\ntype commit\ntag v1.0.0\ntagger Alice <alice@shithub.test> %d +0000\n\nrelease notes\n",
		emptyCommit, time.Now().Unix(),
	)
	var sigBuf bytes.Buffer
	armorWriter, err := armor.Encode(&sigBuf, "PGP SIGNATURE", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := openpgp.DetachSign(armorWriter, entity, strings.NewReader(tagPayload), nil); err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	armorWriter.Close()

	tagBody := []byte(tagPayload + sigBuf.String())

	cmd := exec.Command("git", "-C", gitDir, "hash-object", "-w", "-t", "tag", "--stdin")
	cmd.Stdin = bytes.NewReader(tagBody)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object tag: %v", err)
	}
	oid := strings.TrimSpace(string(out))

	signer := signingSubkey(t, entity)
	emails := []UserEmail{{Email: "alice@shithub.test", Verified: true}}
	lookups := lookupsForEntity(t, entity, signer, true, time.Time{}, emails)

	got, err := VerifyTag(context.Background(), gitDir, oid, lookups)
	if err != nil {
		t.Fatalf("VerifyTag: %v", err)
	}
	if got.Reason != ReasonValid {
		t.Errorf("reason: got %q, want valid", got.Reason)
	}
}

// ─── splitSignedObject unit tests ──────────────────────────────────

func TestSplitSignedObject_Unsigned(t *testing.T) {
	body := []byte("tree abc\nauthor X <x@x> 0 +0000\ncommitter X <x@x> 0 +0000\n\nmessage\n")
	payload, sig, signed := splitSignedObject(body)
	if signed {
		t.Error("expected signed=false")
	}
	if !bytes.Equal(payload, body) {
		t.Errorf("payload mismatch")
	}
	if sig != "" {
		t.Errorf("sig: got %q, want empty", sig)
	}
}

// ─── helpers ────────────────────────────────────────────────────────

// suppress unused-import warning for filepath if it's not referenced.
var _ = filepath.Join
