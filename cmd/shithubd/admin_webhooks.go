// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
)

// adminReencryptWebhooksCmd migrates webhook secret ciphertexts from
// the legacy (TOTP-shared) AEAD key onto the dedicated webhook AEAD
// key (webhook.aead_key). Idempotent: rows already decryptable under
// the primary key are skipped without rewriting.
//
// Run after setting SHITHUB_WEBHOOK__AEAD_KEY in the env and rolling
// the binary out. Until you run this, the deliverer still works
// because OpenSecret tries the legacy key as a fallback — this
// command just retires that fallback path for each row.
//
// Safe to re-run; safe to interrupt; resumable on next invocation
// because rows already on the new key short-circuit.
var adminReencryptWebhooksCmd = &cobra.Command{
	Use:   "re-encrypt-webhooks",
	Short: "Re-encrypt webhook secrets from the legacy TOTP AEAD key to webhook.aead_key",
	Long: `Walks every row in the webhooks table. For each row, tries to
decrypt under the primary (dedicated) webhook AEAD key — if that
works, the row is already migrated and is skipped. Otherwise
decrypts under the legacy (TOTP-shared) key, re-encrypts under
the primary, and writes the new ciphertext + nonce.

Requires SHITHUB_WEBHOOK__AEAD_KEY to be set AND distinct from
SHITHUB_TOTP_KEY. If they're equal, there's nothing to migrate
(the primary IS the legacy key) and the command errors out.

Use --dry-run to count rows that would be re-encrypted without
writing anything.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		pageSize, _ := cmd.Flags().GetInt32("page-size")
		if pageSize <= 0 {
			pageSize = 100
		}

		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("admin re-encrypt-webhooks: DB not configured (set SHITHUB_DATABASE_URL)")
		}
		if cfg.Webhook.AEADKey == "" {
			return errors.New("admin re-encrypt-webhooks: webhook.aead_key not set — nothing to migrate to")
		}
		if cfg.Webhook.AEADKey == cfg.Auth.TOTPKeyB64 {
			return errors.New("admin re-encrypt-webhooks: webhook.aead_key equals auth.totp_key_b64 — no separation to migrate")
		}

		primary, legacy, err := webhook.BuildBoxes(cfg.Webhook.AEADKey, cfg.Auth.TOTPKeyB64)
		if err != nil {
			return fmt.Errorf("build boxes: %w", err)
		}
		if primary == nil {
			return errors.New("admin re-encrypt-webhooks: primary box is nil — refusing to proceed")
		}
		if legacy == nil {
			return errors.New("admin re-encrypt-webhooks: legacy box is nil — no fallback available, rows can't be decrypted")
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()

		pool, err := db.Open(ctx, db.Config{
			URL: cfg.DB.URL, MaxConns: 4, MinConns: 0,
			ConnectTimeout: cfg.DB.ConnectTimeout,
		})
		if err != nil {
			return fmt.Errorf("db open: %w", err)
		}
		defer pool.Close()

		q := webhookdb.New()
		var (
			lastID         int64
			totalSeen      int
			alreadyPrimary int
			reEncrypted    int
			failed         int
		)
		out := cmd.OutOrStdout()
		errOut := cmd.ErrOrStderr()
		for {
			rows, err := q.ListWebhookSecretsForReencrypt(ctx, pool, webhookdb.ListWebhookSecretsForReencryptParams{
				ID:    lastID,
				Limit: pageSize,
			})
			if err != nil {
				return fmt.Errorf("list webhooks (after id=%d): %w", lastID, err)
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				totalSeen++
				lastID = row.ID
				// Try primary first — already-migrated rows decrypt
				// here on the first try.
				if _, err := primary.Open(row.SecretCiphertext, row.SecretNonce); err == nil {
					alreadyPrimary++
					continue
				}
				// Decrypt under legacy.
				plaintext, err := legacy.Open(row.SecretCiphertext, row.SecretNonce)
				if err != nil {
					failed++
					_, _ = fmt.Fprintf(errOut, "warn: id=%d decrypts under neither key — skipped (%v)\n", row.ID, err)
					continue
				}
				if dryRun {
					reEncrypted++
					continue
				}
				newCT, newNonce, err := primary.Seal(plaintext)
				if err != nil {
					failed++
					_, _ = fmt.Fprintf(errOut, "warn: id=%d re-seal failed — skipped (%v)\n", row.ID, err)
					continue
				}
				if err := q.UpdateWebhookSecret(ctx, pool, webhookdb.UpdateWebhookSecretParams{
					ID:               row.ID,
					SecretCiphertext: newCT,
					SecretNonce:      newNonce,
				}); err != nil {
					failed++
					_, _ = fmt.Fprintf(errOut, "warn: id=%d update failed — skipped (%v)\n", row.ID, err)
					continue
				}
				reEncrypted++
			}
		}

		mode := "MIGRATED"
		if dryRun {
			mode = "DRY-RUN"
		}
		_, _ = fmt.Fprintf(out,
			"re-encrypt-webhooks [%s]: total=%d already_on_primary=%d re_encrypted=%d failed=%d\n",
			mode, totalSeen, alreadyPrimary, reEncrypted, failed)
		if failed > 0 {
			return fmt.Errorf("re-encrypt-webhooks: %d rows failed — investigate before retrying", failed)
		}
		return nil
	},
}

func init() {
	adminReencryptWebhooksCmd.Flags().Bool("dry-run", false, "count rows that would be re-encrypted, write nothing")
	adminReencryptWebhooksCmd.Flags().Int32("page-size", 100, "rows per pagination batch")
	adminCmd.AddCommand(adminReencryptWebhooksCmd)
}
