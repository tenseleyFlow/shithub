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

	"github.com/tenseleyFlow/shithub/internal/actions/cleanup"
	"github.com/tenseleyFlow/shithub/internal/actions/finalize"
	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
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
		objectStore, err := buildWorkerObjectStore(cfg.Storage.S3, logger)
		if err != nil {
			return fmt.Errorf("object storage: %w", err)
		}
		box, boxErr := secretbox.FromBase64(cfg.Auth.TOTPKeyB64)
		if boxErr != nil {
			logger.Warn("secretbox unavailable for encrypted worker payloads",
				"hint", "set Auth.TOTPKeyB64 to a base64 32-byte key",
				"error", boxErr)
		}
		// Webhook AEAD box. Built from cfg.Webhook.AEADKey; nil when
		// unset (delivery disabled below). The pre-separation
		// auth.totp_key_b64 fallback was retired in F-shakedown.
		webhookBox, webhookBoxErr := webhook.BuildBox(cfg.Webhook.AEADKey)
		if webhookBoxErr != nil {
			logger.Warn("webhook AEAD box unavailable",
				"hint", "set SHITHUB_WEBHOOK__AEAD_KEY to a base64 32-byte key",
				"error", webhookBoxErr)
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
				return fmt.Errorf("billing: %w", err)
			}
			stripeRemote = remote
		}

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
		p.Register(worker.KindRepoTemplateInit, jobs.RepoTemplateInit(jobs.TemplateInitDeps{
			Pool: pool, RepoFS: rfs, Logger: logger, ShithubdPath: shithubdPath,
		}))
		p.Register(worker.KindScheduledIssueCreate, jobs.ScheduledIssueCreate(jobs.ScheduledIssueCreateDeps{
			Pool: pool, Logger: logger, Audit: auditRecorder(), IssuesLimiter: throttle.NewLimiter(),
		}))
		p.Register(worker.KindSecretScanHistory, jobs.SecretScanHistory(jobs.SecretScanHistoryDeps{
			Pool: pool, RepoFS: rfs, Logger: logger, Enforce: cfg.Billing.Enforce,
		}))

		p.Register(worker.KindRepoIndexCode, jobs.RepoIndexCode(jobs.IndexCodeDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))
		p.Register(worker.KindRepoIndexReconcile, jobs.RepoIndexReconcile(jobs.IndexReconcileDeps{
			Pool: pool, Logger: logger,
		}))
		importDeps := jobs.OrgGitHubImportDeps{
			Pool: pool, RepoFS: rfs, Box: box, Audit: auditRecorder(),
			Limiter: throttle.NewLimiter(), Logger: logger, ShithubdPath: shithubdPath,
		}
		p.Register(worker.KindOrgGitHubImportDiscover, jobs.OrgGitHubImportDiscover(importDeps))
		p.Register(worker.KindOrgGitHubImportRepo, jobs.OrgGitHubImportRepo(importDeps))
		p.Register(worker.KindOrgBillingSeatSync, jobs.OrgBillingSeatSync(jobs.OrgBillingSeatSyncDeps{
			Pool: pool, Logger: logger, Stripe: stripeRemote,
		}))
		p.Register(worker.KindOrgUsageReconcile, jobs.OrgUsageReconcile(jobs.OrgUsageReconcileDeps{
			Pool: pool, Logger: logger,
		}))
		p.Register(worker.KindOrgUsageRecalc, jobs.OrgUsageRecalc(jobs.OrgUsageRecalcDeps{
			Pool: pool, Logger: logger,
		}))

		p.Register(worker.KindGPGBackfill, jobs.GPGBackfill(jobs.GPGBackfillDeps{
			Pool: pool, RepoFS: rfs, Logger: logger,
		}))

		notifSender, _ := pickNotifEmailSender(cfg)
		p.Register(worker.KindNotifyFanout, jobs.NotifyFanout(jobs.NotifyFanoutDeps{
			Pool:              pool,
			Logger:            logger,
			EmailSender:       notifSender,
			EmailFrom:         cfg.Auth.EmailFrom,
			SiteName:          cfg.Auth.SiteName,
			BaseURL:           cfg.Auth.BaseURL,
			UnsubscribeKey:    notifUnsubscribeKey(cfg, logger),
			EnforceInboxRules: cfg.Billing.Enforce.UserInboxRules,
		}))
		p.Register(worker.KindTrendingCompute, jobs.TrendingCompute(jobs.TrendingComputeDeps{
			Pool: pool, Logger: logger,
		}))

		// Webhook delivery (S33). The fan-out drains domain_events
		// past its own cursor; deliver runs per-row HTTP POSTs;
		// purge-old prunes terminal rows past the retention window.
		// SHITHUB_WEBHOOK__AEAD_KEY is required; absent, delivery is
		// disabled with a loud warning.
		if webhookBox == nil {
			logger.Warn("webhook: no AEAD key configured; webhook delivery disabled",
				"hint", "set SHITHUB_WEBHOOK__AEAD_KEY to a base64 32-byte key")
		} else {
			p.Register(webhook.KindWebhookFanout, jobs.WebhookFanout(jobs.WebhookFanoutDeps{
				Pool: pool, Logger: logger,
			}))
			p.Register(webhook.KindWebhookDeliver, jobs.WebhookDeliver(jobs.WebhookDeliverDeps{
				Pool:      pool,
				Logger:    logger,
				SecretBox: webhookBox,
				SSRF:      webhook.DefaultSSRFConfig(),
			}))
			p.Register(webhook.KindWebhookPurgeOld, jobs.WebhookPurgeOld(jobs.WebhookPurgeOldDeps{
				Pool: pool, Logger: logger, Retention: 30 * 24 * time.Hour,
			}))
			// PRO-EXT01-13a — webhook-relay outbound delivery. Shares
			// the webhook AEAD box (same SHITHUB_WEBHOOK__AEAD_KEY)
			// and SSRF policy as the repo-webhook deliverer.
			p.Register(webhookrelay.KindWebhookRelayDeliver, jobs.WebhookRelayDeliver(jobs.WebhookRelayDeliverDeps{
				Pool:      pool,
				Logger:    logger,
				SecretBox: webhookBox,
				SSRF:      webhook.DefaultSSRFConfig(),
			}))
		}

		// PRO-EXT01-13b — cron-scheduled workflow_dispatch sweep.
		// Operator's systemd timer enqueues KindCronWorkflowSweep on
		// a minute beat; the handler claims due rows + fires each.
		p.Register(cronworkflow.KindCronWorkflowSweep, jobs.CronWorkflowSweep(jobs.CronWorkflowSweepDeps{
			Pool:           pool,
			Logger:         logger,
			RepoFS:         rfs,
			BillingEnforce: cfg.Billing.Enforce,
		}))

		// Actions trigger pipeline (S41b). Discovers .shithub/workflows/
		// at the head sha, parses each, matches against the triggering
		// event, and enqueues workflow_runs for the matches. Runs sit
		// in 'queued' status — no runner picks them up until S41c+.
		p.Register(trigger.KindWorkflowTrigger, trigger.Handler(trigger.JobDeps{
			Deps: trigger.Deps{Pool: pool, Logger: logger}, RepoFS: rfs,
		}))
		if objectStore != nil {
			p.Register(finalize.KindWorkflowFinalizeStep, finalize.Handler(finalize.Deps{
				Pool: pool, ObjectStore: objectStore, Logger: logger,
			}))
		} else {
			logger.Info("actions: object storage not configured; workflow step log finalization disabled")
		}
		p.Register(cleanup.KindWorkflowCleanup, cleanup.Handler(cleanup.Deps{
			Pool: pool, ObjectStore: objectStore, Logger: logger,
		}))

		return p.Run(ctx)
	},
}

func init() {
	workerCmd.Flags().Int("workers", 0, "Number of worker goroutines (default 4)")
	rootCmd.AddCommand(workerCmd)
}

func buildWorkerObjectStore(s config.S3StorageConfig, logger *slog.Logger) (storage.ObjectStore, error) {
	if s.Bucket == "" {
		return nil, nil
	}
	if logger != nil {
		logger.Info("storage: configuring object store for worker", "bucket", s.Bucket)
	}
	return storage.NewS3Store(storage.S3Config{
		Endpoint:        s.Endpoint,
		Region:          s.Region,
		AccessKeyID:     s.AccessKeyID,
		SecretAccessKey: s.SecretAccessKey,
		Bucket:          s.Bucket,
		UseSSL:          s.UseSSL,
		ForcePathStyle:  s.ForcePathStyle,
	})
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
	case "resend":
		return &email.ResendSender{
			APIKey: cfg.Auth.Resend.APIKey,
			From:   cfg.Auth.EmailFrom,
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
