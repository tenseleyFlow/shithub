// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	authh "github.com/tenseleyFlow/shithub/internal/web/handlers/auth"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// usernameLookup returns the lookup function consumed by middleware.OptionalUser.
func usernameLookup(pool *pgxpool.Pool) func(context.Context, int64) (string, error) {
	q := usersdb.New()
	return func(ctx context.Context, id int64) (string, error) {
		u, err := q.GetUserByID(ctx, pool, id)
		if err != nil {
			return "", err
		}
		return u.Username, nil
	}
}

// buildAuthHandlers wires the auth surface from the loaded config and
// the bootstrapped DB / session / logger / templates. Selecting the email
// backend is config-driven (`auth.email_backend`).
func buildAuthHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	store session.Store,
	logger *slog.Logger,
	tmplFS fs.FS,
) (*authh.Handlers, error) {
	rr, err := render.New(tmplFS, render.Options{Octicons: render.BuiltinOcticons()})
	if err != nil {
		return nil, err
	}
	sender, err := pickEmailSender(cfg)
	if err != nil {
		return nil, err
	}
	var box *secretbox.Box
	if cfg.Auth.TOTPKeyB64 != "" {
		b, err := secretbox.FromBase64(cfg.Auth.TOTPKeyB64)
		if err != nil {
			return nil, err
		}
		box = b
	} else {
		logger.Warn("auth: no totp_key_b64 configured; 2FA enrollment routes disabled",
			"hint", "set SHITHUB_TOTP_KEY=$(openssl rand -base64 32) to enable 2FA")
	}
	return authh.New(authh.Deps{
		Logger:       logger,
		Render:       rr,
		Pool:         pool,
		SessionStore: store,
		Email:        sender,
		Branding: email.Branding{
			SiteName: cfg.Auth.SiteName,
			BaseURL:  cfg.Auth.BaseURL,
			From:     cfg.Auth.EmailFrom,
		},
		Argon2: password.Params{
			Memory:  cfg.Auth.Argon2.MemoryKiB,
			Time:    cfg.Auth.Argon2.Time,
			Threads: cfg.Auth.Argon2.Threads,
			SaltLen: 16,
			KeyLen:  32,
		},
		Limiter:                  throttle.NewLimiter(),
		RequireEmailVerification: cfg.Auth.RequireEmailVerification,
		SecretBox:                box,
		Audit:                    audit.NewRecorder(),
	})
}

func pickEmailSender(cfg config.Config) (email.Sender, error) {
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
		return nil, errors.New("auth: unknown email_backend")
	}
}
