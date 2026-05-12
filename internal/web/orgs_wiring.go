// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgshandlers "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// buildOrgHandlers wires the S30 organization handler set. Owns its
// own renderer (same pattern as the search/notif builders).
func buildOrgHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	objectStore storage.ObjectStore,
	tmplFS fs.FS,
	logger *slog.Logger,
) (*orgshandlers.Handlers, error) {
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, err
	}
	var stripeRemote stripebilling.Remote
	if cfg.Billing.Enabled {
		remote, err := stripebilling.New(stripebilling.Config{
			SecretKey:     cfg.Billing.Stripe.SecretKey,
			WebhookSecret: cfg.Billing.Stripe.WebhookSecret,
			TeamPriceID:   cfg.Billing.Stripe.TeamPriceID,
			AutomaticTax:  cfg.Billing.Stripe.AutomaticTax,
		})
		if err != nil {
			return nil, err
		}
		stripeRemote = remote
	}
	sender, _ := pickOrgsEmailSender(cfg)
	var box *secretbox.Box
	if cfg.Auth.TOTPKeyB64 != "" {
		if b, err := secretbox.FromBase64(cfg.Auth.TOTPKeyB64); err == nil {
			box = b
		} else if logger != nil {
			logger.Warn("orgs: actions secretbox unavailable",
				"hint", "set Auth.TOTPKeyB64 to a base64 32-byte key",
				"error", err)
		}
	}
	return orgshandlers.New(orgshandlers.Deps{
		Logger:                logger,
		Render:                rr,
		Pool:                  pool,
		EmailSender:           sender,
		EmailFrom:             cfg.Auth.EmailFrom,
		SiteName:              cfg.Auth.SiteName,
		BaseURL:               cfg.Auth.BaseURL,
		ObjectStore:           objectStore,
		SecretBox:             box,
		Audit:                 audit.NewRecorder(),
		BillingEnabled:        cfg.Billing.Enabled,
		BillingGracePeriod:    cfg.Billing.GracePeriod,
		Stripe:                stripeRemote,
		StripeSuccessURL:      cfg.Billing.Stripe.SuccessURL,
		StripeCancelURL:       cfg.Billing.Stripe.CancelURL,
		StripePortalReturnURL: cfg.Billing.Stripe.PortalReturnURL,
	})
}

// pickOrgsEmailSender mirrors pickEmailSender in auth_wiring.go.
// Kept local so a missing/misconfigured email backend doesn't crash
// the org surface — invitations still land in the DB; the email side
// is best-effort.
func pickOrgsEmailSender(cfg config.Config) (email.Sender, error) {
	switch cfg.Auth.EmailBackend {
	case "stdout":
		return email.NewStdoutSender(os.Stdout), nil
	case "smtp":
		return &email.SMTPSender{
			Addr:     cfg.Auth.SMTP.Addr,
			From:     cfg.Auth.EmailFrom,
			Username: cfg.Auth.SMTP.Username,
			Password: cfg.Auth.SMTP.Password,
		}, nil
	case "postmark":
		return &email.PostmarkSender{
			ServerToken: cfg.Auth.Postmark.ServerToken,
			From:        cfg.Auth.EmailFrom,
			HTTP:        &http.Client{Timeout: 10 * time.Second},
		}, nil
	default:
		return nil, errors.New("orgs: unknown email_backend")
	}
}
