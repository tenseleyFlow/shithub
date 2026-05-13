// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/runnerjwt"
	"github.com/tenseleyFlow/shithub/internal/auth/sealbox"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
	authh "github.com/tenseleyFlow/shithub/internal/web/handlers/auth"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
	"github.com/tenseleyFlow/shithub/internal/webhook"
)

// sharedPATDebouncer is used by both the PATAuth middleware (in the API
// route group) and any future paths that want to coordinate last-used
// updates across handlers in the same process.
var sharedPATDebouncer = pat.NewDebouncer(0)

// buildAPIHandlers wires the PAT-authenticated API surface.
func buildAPIHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	objectStore storage.ObjectStore,
	runnerJWT *runnerjwt.Signer,
	secretBox *secretbox.Box,
	rateLimiter *ratelimit.Limiter,
	logger *slog.Logger,
) (*apih.Handlers, error) {
	if cfg.Storage.ReposRoot == "" {
		return nil, errors.New("api: cfg.Storage.ReposRoot is empty")
	}
	root, err := filepath.Abs(cfg.Storage.ReposRoot)
	if err != nil {
		return nil, fmt.Errorf("api: resolve repos_root: %w", err)
	}
	rfs, err := storage.NewRepoFS(root)
	if err != nil {
		return nil, fmt.Errorf("api: NewRepoFS: %w", err)
	}
	shithubdPath := "shithubd"
	if exe, err := os.Executable(); err == nil {
		if abs, absErr := filepath.Abs(exe); absErr == nil {
			shithubdPath = abs
		}
	}
	// X25519 sealed-box keypair for the REST `actions/secrets/public-key`
	// endpoint. Loaded from config when present; auto-generated with a
	// loud warning otherwise (dev convenience — production deployments
	// MUST set the env knob so secrets survive process restart).
	var secretsBox *sealbox.Box
	if pk := cfg.Actions.Secrets.BoxPrivateKeyB64; pk != "" {
		b, err := sealbox.FromBase64(pk)
		if err != nil {
			return nil, fmt.Errorf("api: actions secrets box: %w", err)
		}
		secretsBox = b
	} else {
		b, err := sealbox.New()
		if err != nil {
			return nil, fmt.Errorf("api: actions secrets box (auto): %w", err)
		}
		logger.Warn("actions secrets box: auto-generated keypair; secrets PUT against this process will not be decryptable after restart",
			"hint", "set SHITHUB_ACTIONS__SECRETS__BOX_PRIVATE_KEY_B64=$(openssl rand -base64 32) for persistence")
		secretsBox = b
	}
	return apih.New(apih.Deps{
		Pool:         pool,
		Debouncer:    sharedPATDebouncer,
		Logger:       logger,
		ObjectStore:  objectStore,
		RepoFS:       rfs,
		RunnerJWT:    runnerJWT,
		SecretBox:    secretBox,
		SecretsBox:   secretsBox,
		RateLimiter:  rateLimiter,
		Audit:        audit.NewRecorder(),
		Throttle:     throttle.NewLimiter(),
		ShithubdPath: shithubdPath,
		BaseURL:      cfg.Auth.BaseURL,
		APILimit: apilimit.Config{
			AuthedPerHour: cfg.RateLimit.API.AuthedPerHour,
			AnonPerHour:   cfg.RateLimit.API.AnonPerHour,
			Logger:        logger,
		},
		WebhookSSRF: webhook.DefaultSSRFConfig(),
	})
}

// usernameLookup returns the lookup function consumed by middleware.OptionalUser.
// It resolves the username, the user's current session_epoch (so the auth
// middleware can refuse stale cookies bumped by "log out everywhere"), and
// the suspended state (so policy.UserActor can deny writes by suspended
// accounts at the web layer — the audit found this gap, S00-S25 audit C1).
func usernameLookup(pool *pgxpool.Pool) middleware.UserLookup {
	q := usersdb.New()
	return func(ctx context.Context, id int64) (middleware.UserLookupResult, error) {
		u, err := q.GetUserByID(ctx, pool, id)
		if err != nil {
			return middleware.UserLookupResult{}, err
		}
		return middleware.UserLookupResult{
			Username:     u.Username,
			SessionEpoch: u.SessionEpoch,
			IsSuspended:  u.SuspendedAt.Valid,
			IsSiteAdmin:  u.IsSiteAdmin,
		}, nil
	}
}

// buildAuthHandlers wires the auth surface from the loaded config and
// the bootstrapped DB / session / logger / templates. Selecting the email
// backend is config-driven (`auth.email_backend`).
func buildAuthHandlers(
	cfg config.Config,
	pool *pgxpool.Pool,
	store session.Store,
	objectStore storage.ObjectStore,
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
	var stripeRemote stripebilling.Remote
	if cfg.Billing.Enabled {
		remote, err := stripebilling.New(stripebilling.Config{
			SecretKey:     cfg.Billing.Stripe.SecretKey,
			WebhookSecret: cfg.Billing.Stripe.WebhookSecret,
			TeamPriceID:   cfg.Billing.Stripe.TeamPriceID,
			ProPriceID:    cfg.Billing.Stripe.ProPriceID,
			AutomaticTax:  cfg.Billing.Stripe.AutomaticTax,
		})
		if err != nil {
			return nil, err
		}
		stripeRemote = remote
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
		RateLimiter:              ratelimit.New(pool),
		RequireEmailVerification: cfg.Auth.RequireEmailVerification,
		SecretBox:                box,
		Audit:                    audit.NewRecorder(),
		ObjectStore:              objectStore,
		OrgBillingEnabled:        cfg.Billing.Enabled,
		BillingEnabled:           cfg.Billing.Enabled,
		BillingGracePeriod:       cfg.Billing.GracePeriod,
		Stripe:                   stripeRemote,
		StripeSuccessURL:         cfg.Billing.Stripe.SuccessURL,
		StripeCancelURL:          cfg.Billing.Stripe.CancelURL,
		StripePortalReturnURL:    cfg.Billing.Stripe.PortalReturnURL,
		BaseURL:                  cfg.Auth.BaseURL,
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
	case "resend":
		return &email.ResendSender{
			APIKey: cfg.Auth.Resend.APIKey,
			From:   cfg.Auth.EmailFrom,
			HTTP:   &http.Client{Timeout: 10 * time.Second},
		}, nil
	default:
		return nil, errors.New("auth: unknown email_backend")
	}
}
