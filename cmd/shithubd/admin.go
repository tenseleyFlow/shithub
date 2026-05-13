// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	admindb "github.com/tenseleyFlow/shithub/internal/admin/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/token"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
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
	case "resend":
		return &email.ResendSender{
			APIKey: cfg.Auth.Resend.APIKey,
			From:   cfg.Auth.EmailFrom,
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

// adminBootstrapAdminCmd is the chicken-and-egg solver for first
// install: there's no /admin/users/{id} toggle reachable until at
// least one user already has is_site_admin=true. Operators run this
// once on a fresh deploy. Subsequent admin grants happen through the
// /admin UI under an existing admin's session.
var adminBootstrapAdminCmd = &cobra.Command{
	Use:   "bootstrap-admin <username>",
	Short: "Set users.is_site_admin=true on a single user (first-install bootstrap)",
	Long: `Flips the site-admin flag on a user account so the /admin surface
is reachable. Designed for the first-install bootstrap: once any admin
exists, future grants happen in /admin/users/{id}.

Writes an audit row attributed to actor_id=0 (CLI) so the bootstrap
remains traceable.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		username := args[0]
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("admin bootstrap-admin: DB not configured (set SHITHUB_DATABASE_URL)")
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
		uq := usersdb.New()
		user, err := uq.GetUserByUsername(ctx, pool, username)
		if err != nil {
			return fmt.Errorf("user %q not found", username)
		}
		aq := admindb.New()
		if err := aq.SetUserSiteAdmin(ctx, pool, admindb.SetUserSiteAdminParams{
			ID: user.ID, IsSiteAdmin: true,
		}); err != nil {
			return fmt.Errorf("set site admin: %w", err)
		}
		recorder := audit.NewRecorder()
		_ = recorder.Record(ctx, pool, 0,
			audit.ActionAdminSiteAdminGranted, audit.TargetUser, user.ID,
			map[string]any{"via": "cli", "bootstrap": true})
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"bootstrap-admin: %s now has is_site_admin=true\n", user.Username)
		return nil
	},
}

// adminRunJobCmd enqueues an arbitrary worker job. Break-glass for
// operators when the normal trigger path is broken or the job needs a
// one-off invocation.
var adminRunJobCmd = &cobra.Command{
	Use:   "run-job <kind> [payload-json]",
	Short: "Enqueue a worker job by kind + JSON payload",
	Long: `Inserts a row into the jobs queue with the given kind and payload.
Payload defaults to {} when omitted. The worker pool picks the job up
on its next claim cycle (within IdlePoll seconds).`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		kind := args[0]
		payloadJSON := []byte("{}")
		if len(args) == 2 {
			payloadJSON = []byte(args[1])
		}
		var payload map[string]any
		if err := json.Unmarshal(payloadJSON, &payload); err != nil {
			return fmt.Errorf("payload not valid JSON: %w", err)
		}
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("admin run-job: DB not configured")
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
		id, err := worker.Enqueue(ctx, pool, worker.Kind(kind), payload, worker.EnqueueOptions{})
		if err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "run-job: enqueued #%d (kind=%s)\n", id, kind)
		return nil
	},
}

// adminRecomputeCmd triggers a counter reconciler. Useful when a
// star_count / fork_count drifts from the source-of-truth row count.
// Currently supports `star_count` and `fork_count`; new metrics get
// new branches here.
var adminRecomputeCmd = &cobra.Command{
	Use:   "recompute <metric>",
	Short: "Recompute a denormalized counter (star_count, fork_count)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		metric := args[0]
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("admin recompute: DB not configured")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
			ConnectTimeout: cfg.DB.ConnectTimeout,
		})
		if err != nil {
			return fmt.Errorf("db open: %w", err)
		}
		defer pool.Close()
		var sql string
		switch metric {
		case "star_count":
			sql = `UPDATE repos r
			       SET star_count = COALESCE(s.n, 0)
			       FROM (SELECT repo_id, COUNT(*)::bigint AS n FROM stars GROUP BY repo_id) s
			       WHERE r.id = s.repo_id`
		case "fork_count":
			sql = `UPDATE repos r
			       SET fork_count = COALESCE(f.n, 0)
			       FROM (SELECT fork_of_repo_id AS rid, COUNT(*)::bigint AS n
			             FROM repos WHERE fork_of_repo_id IS NOT NULL GROUP BY fork_of_repo_id) f
			       WHERE r.id = f.rid`
		default:
			return fmt.Errorf("unknown metric %q (want star_count | fork_count)", metric)
		}
		tag, err := pool.Exec(ctx, sql)
		if err != nil {
			return fmt.Errorf("recompute: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"recompute: %s — %d rows updated\n", metric, tag.RowsAffected())
		return nil
	},
}

func init() {
	adminCmd.AddCommand(adminResetPasswordCmd)
	adminCmd.AddCommand(adminClear2FACmd)
	adminCmd.AddCommand(adminBootstrapAdminCmd)
	adminCmd.AddCommand(adminRunJobCmd)
	adminCmd.AddCommand(adminRecomputeCmd)
}
