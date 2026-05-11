// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"encoding/base32"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/totp"
)

// base32StdLowerStrict is the standard base32 alphabet used by EncodeBase32.
var base32StdLowerStrict = base32.StdEncoding

// enrollTOTPHelper completes the signup → enable-2FA flow and returns
// everything subsequent tests need to assert against the live server.
func enrollTOTPHelper(t *testing.T, requireVerify bool) (
	cli *client,
	captor *captureSender,
	username string,
	password string,
	secret []byte,
	recovery []string,
) {
	t.Helper()
	httpsrv, cap := newTestServer(t, requireVerify)
	captor = cap
	cli = newClient(t, httpsrv)

	username = "alice2fa"
	password = "correct horse battery staple"
	mustSignup(t, cli, username, "alice2fa@example.com", password)

	// Verify email so login works regardless of requireVerify.
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	resp := cli.get(t, "/verify-email/"+tok)
	_ = resp.Body.Close()

	// First login — no 2FA yet, so this lands on / .
	csrf := cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {username}, "password": {password},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first login: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// GET enable form — server mints a fresh secret and sends it inline
	// (test template emits SECRET=<base32> in the body).
	resp = cli.get(t, "/settings/security/2fa/enable")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("2fa enable form: %d %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	secretRE := regexp.MustCompile(`SECRET=([A-Z2-7]+)`)
	m := secretRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no secret in enable form body: %s", body)
	}
	secret = decodeBase32(t, m[1])

	// Compute current TOTP code from the secret and submit it.
	csrf = extractCSRFFromBody(t, body)
	code, err := totp.Generate(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	resp = cli.post(t, "/settings/security/2fa/enable", url.Values{
		"csrf_token": {csrf}, "code": {code},
	})
	if resp.StatusCode != http.StatusOK {
		body2, _ := io.ReadAll(resp.Body)
		t.Fatalf("2fa confirm: %d %s", resp.StatusCode, body2)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	codesRE := regexp.MustCompile(`CODES=([A-Z0-9\-;]+)`)
	cm := codesRE.FindStringSubmatch(string(body))
	if cm == nil {
		t.Fatalf("no recovery codes in body: %s", body)
	}
	recovery = strings.Split(strings.TrimSuffix(cm[1], ";"), ";")

	return cli, captor, username, password, secret, recovery
}

// extractCSRFFromBody finds the csrf token in an already-fetched body.
func extractCSRFFromBody(t *testing.T, body []byte) string {
	t.Helper()
	m := csrfMarkerRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no csrf marker in body: %s", body)
	}
	return htmlUnescape(m[1])
}

// htmlUnescape mirrors what extractCSRF does internally.
func htmlUnescape(s string) string {
	return strings.NewReplacer("&#43;", "+", "&#47;", "/", "&#61;", "=").Replace(s)
}

func decodeBase32(t *testing.T, s string) []byte {
	t.Helper()
	// pad to multiple of 8.
	for len(s)%8 != 0 {
		s += "="
	}
	b, err := base32StdLowerStrict.DecodeString(s)
	if err != nil {
		t.Fatalf("base32 decode: %v", err)
	}
	return b
}

// ============================== tests ==================================

func TestTwoFactor_Enroll_Logout_Login_Challenge_FullSession(t *testing.T) {
	t.Parallel()
	cli, _, username, password, secret, _ := enrollTOTPHelper(t, false)

	// Logout.
	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/logout", url.Values{"csrf_token": {csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Re-login with password — should redirect to /login/2fa.
	csrf = cli.extractCSRF(t, "/login")
	resp = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {username}, "password": {password},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login after enroll: %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/login/2fa" {
		t.Fatalf("expected redirect to /login/2fa, got %q", loc)
	}
	_ = resp.Body.Close()

	// Challenge form, submit a TOTP from the NEXT step. Enrollment-confirm
	// already advanced last_used_counter to the current step; with ±1
	// skew, a code from now+30s decodes to step+1 and is accepted.
	csrf = cli.extractCSRF(t, "/login/2fa")
	code, _ := totp.Generate(secret, time.Now().Add(30*time.Second))
	resp = cli.post(t, "/login/2fa", url.Values{"csrf_token": {csrf}, "code": {code}})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("2fa challenge: %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/explore" {
		t.Fatalf("expected /explore after challenge, got %q", loc)
	}
	_ = resp.Body.Close()
}

func TestTwoFactor_RecoveryCode_OneTimeUse(t *testing.T) {
	t.Parallel()
	cli, _, username, password, _, recovery := enrollTOTPHelper(t, false)
	if len(recovery) == 0 {
		t.Fatal("no recovery codes captured")
	}
	code := recovery[0]

	// Logout.
	csrf := cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/logout", url.Values{"csrf_token": {csrf}}).Body.Close()

	// Login + recovery code → session upgraded.
	csrf = cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {username}, "password": {password},
	})
	_ = resp.Body.Close()
	csrf = cli.extractCSRF(t, "/login/2fa")
	resp = cli.post(t, "/login/2fa", url.Values{"csrf_token": {csrf}, "code": {code}})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("recovery first use: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Logout, second use of the same code MUST fail.
	csrf = cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/logout", url.Values{"csrf_token": {csrf}}).Body.Close()
	csrf = cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {username}, "password": {password},
	}).Body.Close()
	csrf = cli.extractCSRF(t, "/login/2fa")
	resp = cli.post(t, "/login/2fa", url.Values{"csrf_token": {csrf}, "code": {code}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery second use: expected 200 (form re-render), got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestTwoFactor_CounterReplayRejected(t *testing.T) {
	t.Parallel()
	cli, _, username, password, secret, _ := enrollTOTPHelper(t, false)

	// Logout.
	csrf := cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/logout", url.Values{"csrf_token": {csrf}}).Body.Close()

	// First login + TOTP from the NEXT step (enrollment already used the
	// current step's counter). With ±1 skew the server accepts now+30s.
	code, _ := totp.Generate(secret, time.Now().Add(30*time.Second))
	csrf = cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {username}, "password": {password},
	}).Body.Close()
	csrf = cli.extractCSRF(t, "/login/2fa")
	resp := cli.post(t, "/login/2fa", url.Values{"csrf_token": {csrf}, "code": {code}})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first 2fa: %d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Logout.
	csrf = cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/logout", url.Values{"csrf_token": {csrf}}).Body.Close()

	// Second login attempt within the SAME 30-second window — same code.
	// Expect REJECTION (counter replay).
	csrf = cli.extractCSRF(t, "/login")
	_ = cli.post(t, "/login", url.Values{
		"csrf_token": {csrf}, "username": {username}, "password": {password},
	}).Body.Close()
	csrf = cli.extractCSRF(t, "/login/2fa")
	resp = cli.post(t, "/login/2fa", url.Values{"csrf_token": {csrf}, "code": {code}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("counter-replay: expected 200 (rejected), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Incorrect code") {
		t.Fatalf("expected 'Incorrect code' message, got: %s", body)
	}
}

func TestTwoFactor_DisableRequiresPasswordAndTOTP(t *testing.T) {
	t.Parallel()
	cli, _, _, password, secret, _ := enrollTOTPHelper(t, false)

	// Wrong password → form re-rendered with error.
	csrf := cli.extractCSRF(t, "/settings/security/2fa/disable")
	resp := cli.post(t, "/settings/security/2fa/disable", url.Values{
		"csrf_token": {csrf},
		"password":   {"wrong"},
		"code":       {"000000"},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("disable wrong creds: %d %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "incorrect") {
		t.Fatalf("expected 'incorrect' in error, got: %s", body)
	}

	// Correct password + correct TOTP → succeed.
	csrf = cli.extractCSRF(t, "/settings/security/2fa/disable")
	code, _ := totp.Generate(secret, time.Now().Add(31*time.Second)) // ensure NEW counter step vs enrollment
	resp = cli.post(t, "/settings/security/2fa/disable", url.Values{
		"csrf_token": {csrf}, "password": {password}, "code": {code},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body2, _ := io.ReadAll(resp.Body)
		t.Fatalf("disable: %d %s", resp.StatusCode, body2)
	}
	_ = resp.Body.Close()
}
