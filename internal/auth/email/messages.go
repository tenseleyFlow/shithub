// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	textTemplate "text/template"
)

// Branding is the per-instance customization for outgoing emails.
type Branding struct {
	SiteName string // e.g. "shithub"
	BaseURL  string // e.g. "https://shithub.example" — no trailing slash
	From     string // e.g. "shithub <noreply@shithub.example>"
}

// Templates are inlined here rather than embedded as files. They're short,
// rarely change, and keeping them in code avoids template-discovery bugs
// in the embed.FS layout. When marketing wants editable email templates
// (S25-ish), promote these to templates/email/*.{html,txt}.
var (
	verifyTextTpl = textTemplate.Must(textTemplate.New("verify.txt").Parse(strings.TrimSpace(`
Welcome to {{.SiteName}}, {{.Username}}!

To finish setting up your account, verify your email address by visiting:

  {{.BaseURL}}/verify-email/{{.Token}}

This link expires in 24 hours.

If you didn't create a {{.SiteName}} account, you can ignore this email.
`)))

	verifyHTMLTpl = template.Must(template.New("verify.html").Parse(strings.TrimSpace(`
<p>Welcome to {{.SiteName}}, <strong>{{.Username}}</strong>!</p>
<p>To finish setting up your account, verify your email address:</p>
<p><a href="{{.BaseURL}}/verify-email/{{.Token}}">Verify email</a></p>
<p>This link expires in 24 hours. If you didn't create a {{.SiteName}} account, you can ignore this email.</p>
`)))

	resetTextTpl = textTemplate.Must(textTemplate.New("reset.txt").Parse(strings.TrimSpace(`
Hello,

We received a request to reset the password for the {{.SiteName}} account
associated with this email address.

To choose a new password, visit:

  {{.BaseURL}}/password/reset/{{.Token}}

This link expires in 1 hour.

If you didn't request a password reset, you can ignore this email — your
password will not change.
`)))

	resetHTMLTpl = template.Must(template.New("reset.html").Parse(strings.TrimSpace(`
<p>Hello,</p>
<p>We received a request to reset the password for the {{.SiteName}} account associated with this email address.</p>
<p><a href="{{.BaseURL}}/password/reset/{{.Token}}">Choose a new password</a></p>
<p>This link expires in 1 hour. If you didn't request a password reset, you can ignore this email — your password will not change.</p>
`)))
)

// noticeBodies maps a notice kind to its (subject, plaintext, html) bodies.
// Each body is run through text/template — only the canonical {{.SiteName}}
// and {{.Username}} variables are exposed.
var noticeBodies = map[string]struct {
	Subject, Text, HTML string
}{
	"2fa_enabled": {
		Subject: "Two-factor authentication enabled on your {{.SiteName}} account",
		Text: `Hi {{.Username}},

Two-factor authentication has just been enabled on your {{.SiteName}} account.
If this wasn't you, sign in immediately and disable 2FA, then change your password.
Recovery codes are stored on the security settings page — keep them somewhere safe.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>Two-factor authentication has just been enabled on your {{.SiteName}} account.</p>
<p>If this wasn't you, sign in immediately and disable 2FA, then change your password.</p>
<p>Recovery codes are stored on the security settings page — keep them somewhere safe.</p>`,
	},
	"2fa_disabled": {
		Subject: "Two-factor authentication disabled on your {{.SiteName}} account",
		Text: `Hi {{.Username}},

Two-factor authentication has been disabled on your {{.SiteName}} account.
If this wasn't you, sign in immediately and re-enable 2FA, then change your password.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>Two-factor authentication has been disabled on your {{.SiteName}} account.</p>
<p>If this wasn't you, sign in immediately and re-enable 2FA, then change your password.</p>`,
	},
	"recovery_regenerated": {
		Subject: "New recovery codes generated for your {{.SiteName}} account",
		Text: `Hi {{.Username}},

Your {{.SiteName}} recovery codes were regenerated. Any previous codes
no longer work. Store the new codes somewhere safe.

If this wasn't you, sign in immediately and review your security settings.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>Your {{.SiteName}} recovery codes were regenerated. Any previous codes no longer work. Store the new codes somewhere safe.</p>
<p>If this wasn't you, sign in immediately and review your security settings.</p>`,
	},
	"admin_cleared_2fa": {
		Subject: "Two-factor authentication cleared by support — {{.SiteName}}",
		Text: `Hi {{.Username}},

A {{.SiteName}} administrator cleared two-factor authentication from your
account, typically as part of a support request you initiated.

Sign in and re-enable 2FA at /settings/security/2fa/enable as soon as you can.

If you did NOT request this, sign in immediately and reset your password,
then contact support.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>A {{.SiteName}} administrator cleared two-factor authentication from your account, typically as part of a support request you initiated.</p>
<p>Sign in and re-enable 2FA as soon as you can.</p>
<p>If you did NOT request this, sign in immediately and reset your password, then contact support.</p>`,
	},
	"password_changed": {
		Subject: "Your {{.SiteName}} password was changed",
		Text: `Hi {{.Username}},

Your {{.SiteName}} password was just changed from the account settings.
All other sessions have been signed out as a precaution.

If this wasn't you, reset your password immediately at /password/reset
and review the security log under /settings.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>Your {{.SiteName}} password was just changed from the account settings. All other sessions have been signed out as a precaution.</p>
<p>If this wasn't you, reset your password immediately and review your security log.</p>`,
	},
	"log_out_everywhere": {
		Subject: "All other sessions signed out — {{.SiteName}}",
		Text: `Hi {{.Username}},

You signed out of every other session on your {{.SiteName}} account.
Your current browser stays signed in.

If this wasn't you, change your password immediately and review your
security settings.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>You signed out of every other session on your {{.SiteName}} account. Your current browser stays signed in.</p>
<p>If this wasn't you, change your password immediately and review your security settings.</p>`,
	},
	"username_changed": {
		Subject: "Your {{.SiteName}} username was changed",
		Text: `Hi {{.Username}},

Your {{.SiteName}} username was just changed. The old name now redirects
to the new one for 30 days, then is released.

If this wasn't you, sign in and review your account immediately.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>Your {{.SiteName}} username was just changed. The old name now redirects to the new one for 30 days, then is released.</p>
<p>If this wasn't you, sign in and review your account immediately.</p>`,
	},
	"primary_email_changed": {
		Subject: "Primary email changed on your {{.SiteName}} account",
		Text: `Hi {{.Username}},

The primary email on your {{.SiteName}} account was just changed.
This is the address account-related notifications go to from now on.

If this wasn't you, sign in and review your security settings.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>The primary email on your {{.SiteName}} account was just changed. This is the address account-related notifications go to from now on.</p>
<p>If this wasn't you, sign in and review your security settings.</p>`,
	},
	"account_deletion_initiated": {
		Subject: "Your {{.SiteName}} account was scheduled for deletion",
		Text: `Hi {{.Username}},

Your {{.SiteName}} account has been deleted. You have 14 days to undo this
by signing in again with your existing username and password — that
restores the account in place.

After 14 days the account stays permanently deleted.

If this wasn't you, sign in immediately to restore the account and then
change your password.`,
		HTML: `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>Your {{.SiteName}} account has been deleted. You have <strong>14 days</strong> to undo this by signing in again with your existing username and password — that restores the account in place.</p>
<p>After 14 days the account stays permanently deleted.</p>
<p>If this wasn't you, sign in immediately to restore the account and then change your password.</p>`,
	},
}

// TokenCreatedMessage notifies the user that a new PAT was minted on
// their account. Helps detect compromise — a token they didn't make is a
// big red flag.
func TokenCreatedMessage(b Branding, to, username, name, prefix, ip string) (Message, error) {
	const text = `Hi {{.Username}},

A new personal access token was created on your {{.SiteName}} account.

  Name: {{.Name}}
  Prefix: {{.Prefix}}…
{{if .IP}}  IP: {{.IP}}
{{end}}
If this wasn't you, sign in immediately, revoke the token at
{{.BaseURL}}/settings/tokens, and reset your password.`
	const html = `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>A new personal access token was created on your {{.SiteName}} account.</p>
<ul>
  <li><strong>Name:</strong> {{.Name}}</li>
  <li><strong>Prefix:</strong> <code>{{.Prefix}}…</code></li>
  {{if .IP}}<li><strong>IP:</strong> {{.IP}}</li>{{end}}
</ul>
<p>If this wasn't you, sign in immediately, revoke the token at
<a href="{{.BaseURL}}/settings/tokens">your tokens settings</a>, and reset your password.</p>`
	data := struct{ SiteName, BaseURL, Username, Name, Prefix, IP string }{
		b.SiteName, b.BaseURL, username, name, prefix, ip,
	}
	txt, err := renderText(textTemplate.Must(textTemplate.New("token_added.txt").Parse(text)), data)
	if err != nil {
		return Message{}, err
	}
	htmlBody, err := renderHTML(template.Must(template.New("token_added.html").Parse(html)), data)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    b.From,
		To:      to,
		Subject: fmt.Sprintf("New personal access token on your %s account", b.SiteName),
		Text:    txt,
		HTML:    htmlBody,
	}, nil
}

// SSHKeyAddedMessage builds the new-key notification email. Sent on
// successful key add — title/fingerprint/IP help the user spot a
// compromise quickly.
func SSHKeyAddedMessage(b Branding, to, username, title, fingerprint, ip string) (Message, error) {
	const text = `Hi {{.Username}},

A new SSH key was added to your {{.SiteName}} account.

  Title: {{.Title}}
  Fingerprint: {{.Fingerprint}}
{{if .IP}}  IP: {{.IP}}
{{end}}
If this wasn't you, sign in immediately, delete the key from
{{.BaseURL}}/settings/keys, and reset your password.`

	const html = `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>A new SSH key was added to your {{.SiteName}} account.</p>
<ul>
  <li><strong>Title:</strong> {{.Title}}</li>
  <li><strong>Fingerprint:</strong> <code>{{.Fingerprint}}</code></li>
  {{if .IP}}<li><strong>IP:</strong> {{.IP}}</li>{{end}}
</ul>
<p>If this wasn't you, sign in immediately, delete the key from
<a href="{{.BaseURL}}/settings/keys">your SSH keys settings</a>, and reset your password.</p>`

	data := struct{ SiteName, BaseURL, Username, Title, Fingerprint, IP string }{
		b.SiteName, b.BaseURL, username, title, fingerprint, ip,
	}
	txt, err := renderText(textTemplate.Must(textTemplate.New("ssh_added.txt").Parse(text)), data)
	if err != nil {
		return Message{}, err
	}
	htmlBody, err := renderHTML(template.Must(template.New("ssh_added.html").Parse(html)), data)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    b.From,
		To:      to,
		Subject: fmt.Sprintf("New SSH key added to your %s account", b.SiteName),
		Text:    txt,
		HTML:    htmlBody,
	}, nil
}

// GPGKeyAddedMessage builds the new-GPG-key notification email. Sent
// on successful key add — name/fingerprint/IP help the user spot a
// compromise. Mirrors SSHKeyAddedMessage; the only visible difference
// in the body is the wording ("GPG key" vs "SSH key") and the
// settings link target.
//
// The display "name" comes from the user-supplied title field; when
// the user omits it (gh allows blank), we fall back to a generic
// "(no title)" label so the email body stays readable.
func GPGKeyAddedMessage(b Branding, to, username, name, fingerprint, ip string) (Message, error) {
	if strings.TrimSpace(name) == "" {
		name = "(no title)"
	}
	const text = `Hi {{.Username}},

A new GPG key was added to your {{.SiteName}} account.

  Name: {{.Name}}
  Fingerprint: {{.Fingerprint}}
{{if .IP}}  IP: {{.IP}}
{{end}}
If this wasn't you, sign in immediately, delete the key from
{{.BaseURL}}/settings/keys, and reset your password.`

	const html = `<p>Hi <strong>{{.Username}}</strong>,</p>
<p>A new GPG key was added to your {{.SiteName}} account.</p>
<ul>
  <li><strong>Name:</strong> {{.Name}}</li>
  <li><strong>Fingerprint:</strong> <code>{{.Fingerprint}}</code></li>
  {{if .IP}}<li><strong>IP:</strong> {{.IP}}</li>{{end}}
</ul>
<p>If this wasn't you, sign in immediately, delete the key from
<a href="{{.BaseURL}}/settings/keys">your SSH and GPG keys settings</a>, and reset your password.</p>`

	data := struct{ SiteName, BaseURL, Username, Name, Fingerprint, IP string }{
		b.SiteName, b.BaseURL, username, name, fingerprint, ip,
	}
	txt, err := renderText(textTemplate.Must(textTemplate.New("gpg_added.txt").Parse(text)), data)
	if err != nil {
		return Message{}, err
	}
	htmlBody, err := renderHTML(template.Must(template.New("gpg_added.html").Parse(html)), data)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    b.From,
		To:      to,
		Subject: fmt.Sprintf("New GPG key added to your %s account", b.SiteName),
		Text:    txt,
		HTML:    htmlBody,
	}, nil
}

// NoticeMessage builds a 2FA / security state-change notice for kind. The
// kind names match audit-log Action values where applicable.
func NoticeMessage(b Branding, to, username, kind string) (Message, error) {
	body, ok := noticeBodies[kind]
	if !ok {
		return Message{}, fmt.Errorf("email: unknown notice kind %q", kind)
	}
	data := struct{ SiteName, Username string }{b.SiteName, username}
	subj, err := renderText(textTemplate.Must(textTemplate.New("subj").Parse(body.Subject)), data)
	if err != nil {
		return Message{}, err
	}
	txt, err := renderText(textTemplate.Must(textTemplate.New("txt").Parse(body.Text)), data)
	if err != nil {
		return Message{}, err
	}
	html, err := renderHTML(template.Must(template.New("html").Parse(body.HTML)), data)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    b.From,
		To:      to,
		Subject: strings.TrimSpace(subj),
		Text:    txt,
		HTML:    html,
	}, nil
}

// VerifyMessage builds the email-verification message.
func VerifyMessage(b Branding, to, username, token string) (Message, error) {
	data := struct{ SiteName, BaseURL, Username, Token string }{b.SiteName, b.BaseURL, username, token}
	text, err := renderText(verifyTextTpl, data)
	if err != nil {
		return Message{}, err
	}
	html, err := renderHTML(verifyHTMLTpl, data)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    b.From,
		To:      to,
		Subject: fmt.Sprintf("Verify your %s email", b.SiteName),
		HTML:    html,
		Text:    text,
	}, nil
}

// ResetMessage builds the password-reset message.
func ResetMessage(b Branding, to, token string) (Message, error) {
	data := struct{ SiteName, BaseURL, Token string }{b.SiteName, b.BaseURL, token}
	text, err := renderText(resetTextTpl, data)
	if err != nil {
		return Message{}, err
	}
	html, err := renderHTML(resetHTMLTpl, data)
	if err != nil {
		return Message{}, err
	}
	return Message{
		From:    b.From,
		To:      to,
		Subject: fmt.Sprintf("Reset your %s password", b.SiteName),
		HTML:    html,
		Text:    text,
	}, nil
}

func renderText(t *textTemplate.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderHTML(t *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
