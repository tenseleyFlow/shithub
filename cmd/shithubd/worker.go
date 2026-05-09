// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	"github.com/tenseleyFlow/shithub/internal/worker"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

// auditRecorder returns the shared audit recorder. Kept as a function
// rather than a package-level value so future tests / non-default
// recorders can substitute via dependency injection.
func auditRecorder() *audit.Recorder { return audit.NewRecorder() }

// workerCmd boots a long-running worker pool. SIGINT/SIGTERM trigger
// graceful shutdown: the LISTEN goroutine drops, claim attempts stop,
// in-flight jobs are given a deadline to finish, then the binary exits.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run background workers (push processing, size recalc, purge)",
	RunE: func(cmd *cobra.Command, _ []string) error {
		workersFlag, _ := cmd.Flags().GetInt("workers")

		cfg, err := config.Load(nil)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if cfg.DB.URL == "" {
			return errors.New("worker: SHITHUB_DATABASE_URL unset")
		}
		root, err := filepath.Abs(cfg.Storage.ReposRoot)
		if err != nil {
			return fmt.Errorf("repos_root: %w", err)
		}
		rfs, err := storage.NewRepoFS(root)
		if err != nil {
			return fmt.Errorf("repo fs: %w", err)
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Worker count: flag overrides env override default.
		count := workersFlag
		if count <= 0 {
			if v, _ := strconv.Atoi(os.Getenv("SHITHUB_WORKERS")); v > 0 {
				count = v
			}
		}

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: int32(count) + 2, MinConns: 1,
		})
		if err != nil {
			return fmt.Errorf("db: %w", err)
		}
		defer pool.Close()

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

		p := worker.NewPool(pool, worker.PoolConfig{
			Workers:    count,
			IdlePoll:   5 * time.Second,
			JobTimeout: 5 * time.Minute,
			Logger:     logger,
		})
		p.Register(worker.KindPushProcess, jobs.PushProcess(jobs.PushProcessDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))
		p.Register(worker.KindRepoSizeRecalc, jobs.RepoSizeRecalc(jobs.RepoSizeRecalcDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))
		p.Register(worker.KindJobsPurge, jobs.JobsPurge(jobs.JobsPurgeDeps{
			Pool: pool, Logger: logger,
		}))
		p.Register(worker.KindLifecycleSweep, jobs.LifecycleSweep(jobs.LifecycleSweepDeps{
			Pool: pool, RepoFS: rfs, Audit: auditRecorder(), Logger: logger,
		}))
		prDeps := jobs.PRJobsDeps{Pool: pool, RepoFS: rfs, Logger: logger}
		p.Register(worker.KindPRSynchronize, jobs.PRSynchronize(prDeps))
		p.Register(worker.KindPRMergeability, jobs.PRMergeability(prDeps))

		shithubdPath, _ := shithubdBinaryPath()
		p.Register(worker.KindRepoForkClone, jobs.RepoForkClone(jobs.ForkCloneDeps{
			Pool: pool, RepoFS: rfs, Logger: logger, ShithubdPath: shithubdPath,
		}))

		p.Register(worker.KindRepoIndexCode, jobs.RepoIndexCode(jobs.IndexCodeDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))
		p.Register(worker.KindRepoIndexReconcile, jobs.RepoIndexReconcile(jobs.IndexReconcileDeps{
			Pool: pool, Logger: logger,
		}))

		notifSender, _ := pickNotifEmailSender(cfg)
		p.Register(worker.KindNotifyFanout, jobs.NotifyFanout(jobs.NotifyFanoutDeps{
			Pool:           pool,
			Logger:         logger,
			EmailSender:    notifSender,
			EmailFrom:      cfg.Auth.EmailFrom,
			SiteName:       cfg.Auth.SiteName,
			BaseURL:        cfg.Auth.BaseURL,
			UnsubscribeKey: notifUnsubscribeKey(cfg, logger),
		}))

		// Webhook delivery (S33). The fan-out drains domain_events
		// past its own cursor; deliver runs per-row HTTP POSTs;
		// purge-old prunes terminal rows past the retention window.
		// We reuse the TOTP key as the at-rest secretbox key — both
		// are encrypted-blob columns in the same trust domain.
		hookBox, hookBoxErr := secretbox.FromBase64(cfg.Auth.TOTPKeyB64)
		if hookBoxErr != nil {
			logger.Warn("webhook: secretbox unavailable; webhook delivery disabled",
				"hint", "set Auth.TOTPKeyB64 to a base64 32-byte key",
				"error", hookBoxErr)
		} else {
			p.Register(webhook.KindWebhookFanout, jobs.WebhookFanout(jobs.WebhookFanoutDeps{
				Pool: pool, Logger: logger,
			}))
			p.Register(webhook.KindWebhookDeliver, jobs.WebhookDeliver(jobs.WebhookDeliverDeps{
				Pool:      pool,
				Logger:    logger,
				SecretBox: hookBox,
				SSRF:      webhook.DefaultSSRFConfig(),
			}))
			p.Register(webhook.KindWebhookPurgeOld, jobs.WebhookPurgeOld(jobs.WebhookPurgeOldDeps{
				Pool: pool, Logger: logger, Retention: 30 * 24 * time.Hour,
			}))
		}

		return p.Run(ctx)
	},
}

func init() {
	workerCmd.Flags().Int("workers", 0, "Number of worker goroutines (default 4)")
	rootCmd.AddCommand(workerCmd)
}

// pickNotifEmailSender mirrors pickAdminEmailSender / pickEmailSender
// in the web binary. Kept local to the worker so failure to construct
// the sender doesn't kill the process — fan-out without email is a
// supported degraded mode (inbox rows still land).
func pickNotifEmailSender(cfg config.Config) (email.Sender, error) {
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
		return nil, errors.New("worker: unknown email_backend")
	}
}

// notifUnsubscribeKey resolves the HMAC key used to sign one-click
// unsubscribe URLs. Operators set Notif.UnsubscribeKeyB64 in prod.
// In dev (key empty) we derive a deterministic 32-byte key from the
// session secret so unsubscribe links survive process restarts
// without operator action — and we log a loud warning so the
// derivation can't sneak into prod by accident.
func notifUnsubscribeKey(cfg config.Config, logger *slog.Logger) []byte {
	if cfg.Notif.UnsubscribeKeyB64 != "" {
		k, err := base64.StdEncoding.DecodeString(cfg.Notif.UnsubscribeKeyB64)
		if err == nil && len(k) >= 16 {
			return k
		}
		if logger != nil {
			logger.Warn("notif: unsubscribe_key_b64 invalid; falling back to derived key",
				"hint", "set Notif.UnsubscribeKeyB64 to base64-encoded 32+ random bytes")
		}
	}
	if cfg.Session.KeyB64 != "" {
		seed, err := base64.StdEncoding.DecodeString(cfg.Session.KeyB64)
		if err == nil && len(seed) > 0 {
			sum := sha256.Sum256(append([]byte("notif-unsub:"), seed...))
			if logger != nil {
				logger.Warn("notif: deriving unsubscribe key from session secret (dev fallback)",
					"hint", "set Notif.UnsubscribeKeyB64 in prod")
			}
			return sum[:]
		}
	}
	// Last-resort static dev key — links work but anyone with source
	// access can mint them. Logged at WARN so operators notice in
	// prod logs.
	if logger != nil {
		logger.Warn("notif: no key material — using static dev key",
			"hint", "set Notif.UnsubscribeKeyB64 (or session.key_b64)")
	}
	return []byte("shithub-dev-unsub-static-key-32B")
}
