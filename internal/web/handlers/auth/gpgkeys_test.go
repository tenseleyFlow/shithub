// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// enrollGPGKeyHelper signs a fresh user in and returns the test
// client. Mirrors enrollSSHKeyHelper from sshkeys_test.go.
func enrollGPGKeyHelper(t *testing.T) *client {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)

	mustSignup(t, cli, "alicegpg", "alicegpg@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"alicegpg"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli
}

// armoredPublicKey builds a fresh in-memory ed25519 entity and
// returns its armored public-key block.
func armoredPublicKey(t *testing.T, email string) string {
	t.Helper()
	e, err := openpgp.NewEntity("test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	_ = w.Close()
	return buf.String()
}

// armoredPrivateKey returns an armored PRIVATE key block (a
// rejection-fixture). The body of the export has the secret-key
// packets that the parser refuses.
func armoredPrivateKey(t *testing.T, email string) string {
	t.Helper()
	e, err := openpgp.NewEntity("test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PRIVATE KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatalf("SerializePrivate: %v", err)
	}
	_ = w.Close()
	return buf.String()
}

// gpgKeysRE extracts the GPGKEYS= marker entries from the stub
// templates FS (see authTemplatesFS in auth_test.go). Format per
// entry: <id>:<name>:<key_id>;
var gpgKeysRE = regexp.MustCompile(`GPGKEYS=([^<]*)`)

func TestGPGKey_AddListDelete(t *testing.T) {
	t.Parallel()
	cli := enrollGPGKeyHelper(t)
	pub := armoredPublicKey(t, "alicegpg@example.com")

	// Add via the add-form POST.
	csrf := cli.extractCSRF(t, "/settings/keys/gpg/new")
	resp := cli.post(t, "/settings/keys/gpg", url.Values{
		"csrf_token":  {csrf},
		"title":       {"laptop"},
		"armored_key": {pub},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// List page should now carry the GPG key in GPGKEYS=.
	resp = cli.get(t, "/settings/keys")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := gpgKeysRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no GPGKEYS marker in body: %s", body)
	}
	if !strings.Contains(m[1], ":laptop:") {
		t.Fatalf("title 'laptop' not in GPGKEYS entries: %q", m[1])
	}
	entry := strings.SplitN(m[1], ";", 2)[0]
	parts := strings.SplitN(entry, ":", 3)
	if len(parts) < 3 {
		t.Fatalf("malformed GPGKEYS entry: %q", entry)
	}
	id := parts[0]

	// Delete.
	csrf = extractCSRFFromBody(t, body)
	resp = cli.post(t, "/settings/keys/gpg/"+id+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body2, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete: %d %s", resp.StatusCode, body2)
	}
	_ = resp.Body.Close()

	// List should no longer carry the entry.
	resp = cli.get(t, "/settings/keys")
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m = gpgKeysRE.FindStringSubmatch(string(body))
	if m != nil && strings.Contains(m[1], ":laptop:") {
		t.Errorf("deleted key still present: %q", m[1])
	}
}

func TestGPGKey_RejectPrivateKeyBlock(t *testing.T) {
	t.Parallel()
	cli := enrollGPGKeyHelper(t)
	priv := armoredPrivateKey(t, "alicegpg@example.com")

	csrf := cli.extractCSRF(t, "/settings/keys/gpg/new")
	resp := cli.post(t, "/settings/keys/gpg", url.Values{
		"csrf_token":  {csrf},
		"armored_key": {priv},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Server returns the add form with a flash error baked in.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected re-render of add form (200) with error flash; got %d %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("private key")) {
		t.Errorf("error flash missing 'private key' text; body=%s", body)
	}
}

func TestGPGKey_AcceptsEncryptionOnly(t *testing.T) {
	t.Parallel()
	cli := enrollGPGKeyHelper(t)

	// Build an entity, then strip the primary's signing capability
	// flag so it's encryption-only. gh parity: accept and surface
	// can_sign=false; the parser was changed in S51 to stop rejecting
	// this shape.
	e, err := openpgp.NewEntity("eo", "", "encryptonly@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoRSA, RSABits: 2048,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	for _, id := range e.Identities {
		id.SelfSignature.FlagSign = false
		id.SelfSignature.FlagCertify = false
		id.SelfSignature.FlagEncryptCommunications = true
		id.SelfSignature.FlagEncryptStorage = true
		if err := id.SelfSignature.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey, nil); err != nil {
			t.Fatalf("re-sign: %v", err)
		}
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	_ = e.Serialize(w)
	_ = w.Close()

	csrf := cli.extractCSRF(t, "/settings/keys/gpg/new")
	resp := cli.post(t, "/settings/keys/gpg", url.Values{
		"csrf_token":  {csrf},
		"armored_key": {buf.String()},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("encrypt-only should be accepted (gh parity); got %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()
}

func TestGPGKey_DuplicateRejected(t *testing.T) {
	t.Parallel()
	cli := enrollGPGKeyHelper(t)
	pub := armoredPublicKey(t, "alicegpg@example.com")

	// First add succeeds.
	csrf := cli.extractCSRF(t, "/settings/keys/gpg/new")
	resp := cli.post(t, "/settings/keys/gpg", url.Values{
		"csrf_token":  {csrf},
		"title":       {"first"},
		"armored_key": {pub},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first add: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Second add of the same fingerprint should surface the
	// friendly duplicate error.
	csrf = cli.extractCSRF(t, "/settings/keys/gpg/new")
	resp = cli.post(t, "/settings/keys/gpg", url.Values{
		"csrf_token":  {csrf},
		"title":       {"second"},
		"armored_key": {pub},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected re-render of add form; got %d %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("already registered")) && !strings.Contains(string(body), "already registered") {
		t.Errorf("missing duplicate-key flash; body=%s", body)
	}
}
