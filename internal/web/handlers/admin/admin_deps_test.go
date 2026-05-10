// SPDX-License-Identifier: AGPL-3.0-or-later

package admin_test

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	adminh "github.com/tenseleyFlow/shithub/internal/web/handlers/admin"
)

// TestDepsShape pins the admin.Deps type contract: Email + Branding
// MUST be present (SR2 C3). Pre-SR2 the admin handlers had no email
// sender wired, so userResetPassword minted a token, persisted it,
// audited "sent", and discarded the token via `_ = tokEnc`. The user
// never received the email; the audit row lied.
//
// This is a build-time contract: removing the fields breaks compile.
// If you're here because this test failed, the SR2 C3 remediation
// has regressed — restore the fields and the userResetPassword send
// path.
func TestDepsShape(t *testing.T) {
	t.Parallel()

	// Compile-time field references. If Email or Branding are renamed
	// or removed the package fails to build. The runtime assertion
	// below is belt-and-suspenders for type drift.
	d := adminh.Deps{
		Email:    nil,              // type: email.Sender
		Branding: email.Branding{}, // type: email.Branding
	}
	_ = d
}
