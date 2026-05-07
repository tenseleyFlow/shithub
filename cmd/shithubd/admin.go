// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/token"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Site-admin tooling (operator escape hatches)",
}

var adminResetPasswordCmd = &cobra.Command{
	Use:   "reset-password <username>",
	Short: "Issue a password-reset link to the user's primary email",
	Long: `Generates a fresh password-reset token (1-hour TTL) and sends it
to the user's primary email via the configured email backend. Useful when
a locked-out user can't drive the public reset flow themselves.

Exits non-zero if the user is unknown, has no primary email, or if the
email send fails.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		username := args[0]
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("admin reset-password: DB not configured (set SHITHUB_DATABASE_URL)")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
			ConnectTimeout: cfg.DB.ConnectTimeout,
		})
		if err != nil {
			return fmt.Errorf("db open: %w", err)
		}
		defer pool.Close()

		q := usersdb.New()
		user, err := q.GetUserByUsername(ctx, pool, username)
		if err != nil {
			return fmt.Errorf("user %q not found", username)
		}
		if !user.PrimaryEmailID.Valid {
			return fmt.Errorf("user %q has no primary email on file", username)
		}
		em, err := q.GetUserEmailByID(ctx, pool, user.PrimaryEmailID.Int64)
		if err != nil {
			return fmt.Errorf("primary email lookup: %w", err)
		}

		tokEnc, tokHash, err := token.New()
		if err != nil {
			return fmt.Errorf("token: %w", err)
		}
		expires := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
		if _, err := q.CreatePasswordReset(ctx, pool, usersdb.CreatePasswordResetParams{
			UserID: user.ID, TokenHash: tokHash, ExpiresAt: expires,
		}); err != nil {
			return fmt.Errorf("create reset row: %w", err)
		}

		sender, err := pickAdminEmailSender(cfg)
		if err != nil {
			return err
		}
		msg, err := email.ResetMessage(email.Branding{
			SiteName: cfg.Auth.SiteName,
			BaseURL:  cfg.Auth.BaseURL,
			From:     cfg.Auth.EmailFrom,
		}, string(em.Email), tokEnc)
		if err != nil {
			return fmt.Errorf("build reset email: %w", err)
		}
		if err := sender.Send(ctx, msg); err != nil {
			return fmt.Errorf("send reset email: %w", err)
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "reset-password: link emailed to %s (token expires %s)\n",
			em.Email, expires.Time.Format(time.RFC3339))
		return nil
	},
}

func pickAdminEmailSender(cfg config.Config) (email.Sender, error) {
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
		}, nil
	default:
		return nil, fmt.Errorf("admin: unknown email_backend %q", cfg.Auth.EmailBackend)
	}
}

var adminClear2FACmd = &cobra.Command{
	Use:   "clear-2fa <username>",
	Short: "Clear 2FA enrollment from a user account (support escape hatch)",
	Long: `Removes the user's TOTP enrollment and recovery codes, writes an
audit-log row, and emails the user a notification. Use only when the user
has lost both their authenticator device and their recovery codes —
typically after manual identity verification through a support channel.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		username := args[0]
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("admin clear-2fa: DB not configured (set SHITHUB_DATABASE_URL)")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
			ConnectTimeout: cfg.DB.ConnectTimeout,
		})
		if err != nil {
			return fmt.Errorf("db open: %w", err)
		}
		defer pool.Close()

		q := usersdb.New()
		user, err := q.GetUserByUsername(ctx, pool, username)
		if err != nil {
			return fmt.Errorf("user %q not found", username)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if err := q.DeleteUserTOTP(ctx, tx, user.ID); err != nil {
			return fmt.Errorf("delete totp: %w", err)
		}
		if err := q.DeleteUserRecoveryCodes(ctx, tx, user.ID); err != nil {
			return fmt.Errorf("delete recovery: %w", err)
		}
		recorder := audit.NewRecorder()
		if err := recorder.Record(ctx, tx, 0,
			audit.ActionAdminCleared2FA, audit.TargetUser, user.ID,
			map[string]any{"admin": "cli"}); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		// Best-effort notification email.
		if user.PrimaryEmailID.Valid {
			em, err := q.GetUserEmailByID(ctx, pool, user.PrimaryEmailID.Int64)
			if err == nil {
				sender, err := pickAdminEmailSender(cfg)
				if err == nil {
					msg, err := email.NoticeMessage(email.Branding{
						SiteName: cfg.Auth.SiteName,
						BaseURL:  cfg.Auth.BaseURL,
						From:     cfg.Auth.EmailFrom,
					}, string(em.Email), user.Username, "admin_cleared_2fa")
					if err == nil {
						if err := sender.Send(ctx, msg); err != nil {
							_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warn: notification email failed: %v\n", err)
						}
					}
				}
			}
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"clear-2fa: 2FA + recovery codes cleared for %s; audit row written\n", user.Username)
		return nil
	},
}

func init() {
	adminCmd.AddCommand(adminResetPasswordCmd)
	adminCmd.AddCommand(adminClear2FACmd)
}
