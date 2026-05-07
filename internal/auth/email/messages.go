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
