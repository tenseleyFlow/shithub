// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/actions/runnerlabels"
	"github.com/tenseleyFlow/shithub/internal/actions/runnertoken"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

func newAdminRunnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Register, list, and revoke Actions runners",
	}
	cmd.AddCommand(newAdminRunnerRegisterCmd())
	cmd.AddCommand(newAdminRunnerListCmd())
	cmd.AddCommand(newAdminRunnerRevokeCmd())
	return cmd
}

func newAdminRunnerRegisterCmd() *cobra.Command {
	var name string
	var labelsRaw string
	var capacity int
	cmd := &cobra.Command{
		Use:   "register --name <name> [--labels self-hosted,linux] [--capacity 1]",
		Short: "Register an Actions runner and print its token once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("admin runner register: --name is required")
			}
			labels, err := parseRunnerLabels(labelsRaw)
			if err != nil {
				return err
			}
			if capacity < 1 || capacity > 64 {
				return errors.New("admin runner register: --capacity must be between 1 and 64")
			}

			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "register")
			if err != nil {
				return err
			}
			defer pool.Close()

			token, tokenHash, err := runnertoken.New()
			if err != nil {
				return fmt.Errorf("admin runner register: mint token: %w", err)
			}

			q := actionsdb.New()
			tx, err := pool.Begin(ctx)
			if err != nil {
				return fmt.Errorf("admin runner register: begin: %w", err)
			}
			committed := false
			defer func() {
				if !committed {
					_ = tx.Rollback(ctx)
				}
			}()

			runner, err := q.InsertRunner(ctx, tx, actionsdb.InsertRunnerParams{
				Name:               name,
				Labels:             labels,
				Capacity:           int32(capacity),
				RegisteredByUserID: pgtype.Int8{},
			})
			if err != nil {
				return fmt.Errorf("admin runner register: insert runner: %w", err)
			}
			if _, err := q.InsertRunnerToken(ctx, tx, actionsdb.InsertRunnerTokenParams{
				RunnerID:  runner.ID,
				TokenHash: tokenHash,
				ExpiresAt: pgtype.Timestamptz{},
			}); err != nil {
				return fmt.Errorf("admin runner register: insert token: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("admin runner register: commit: %w", err)
			}
			committed = true
			metrics.ActionsRunnerRegistrationsTotal.Inc()

			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"runner registered\nid: %d\nname: %s\nlabels: %s\ncapacity: %d\ntoken: %s\n\nStore this token now; shithub never shows it again.\n",
				runner.ID, runner.Name, strings.Join(runner.Labels, ","), runner.Capacity, token)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Runner name (letters, numbers, underscore, dash)")
	cmd.Flags().StringVar(&labelsRaw, "labels", "", "Comma-separated runner labels")
	cmd.Flags().IntVar(&capacity, "capacity", 1, "Maximum concurrent jobs this runner may execute")
	return cmd
}

func newAdminRunnerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered Actions runners",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "list")
			if err != nil {
				return err
			}
			defer pool.Close()

			rows, err := actionsdb.New().ListRunners(ctx, pool)
			if err != nil {
				return fmt.Errorf("admin runner list: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tCAPACITY\tLABELS\tLAST_HEARTBEAT")
			for _, r := range rows {
				last := "never"
				if r.LastHeartbeatAt.Valid {
					last = r.LastHeartbeatAt.Time.Format(time.RFC3339)
				}
				_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n",
					r.ID, r.Name, r.Status, r.Capacity, strings.Join(r.Labels, ","), last)
			}
			return tw.Flush()
		},
	}
}

func newAdminRunnerRevokeCmd() *cobra.Command {
	var idRaw string
	cmd := &cobra.Command{
		Use:   "revoke --id <id>",
		Short: "Revoke all registration tokens for an Actions runner",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := strconv.ParseInt(strings.TrimSpace(idRaw), 10, 64)
			if err != nil || id <= 0 {
				return errors.New("admin runner revoke: --id must be a positive integer")
			}
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "revoke")
			if err != nil {
				return err
			}
			defer pool.Close()

			q := actionsdb.New()
			runner, err := q.GetRunnerByID(ctx, pool, id)
			if err != nil {
				return fmt.Errorf("admin runner revoke: runner %d not found", id)
			}
			if err := q.RevokeAllTokensForRunner(ctx, pool, id); err != nil {
				return fmt.Errorf("admin runner revoke: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runner revoked\nid: %d\nname: %s\n", runner.ID, runner.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&idRaw, "id", "", "Runner id")
	return cmd
}

func openAdminRunnerPool(ctx context.Context, cfg config.Config, op string) (*pgxpool.Pool, error) {
	if cfg.DB.URL == "" {
		return nil, fmt.Errorf("admin runner %s: DB not configured (set SHITHUB_DATABASE_URL)", op)
	}
	pool, err := db.Open(ctx, db.Config{
		URL: cfg.DB.URL, MaxConns: 2, MinConns: 0,
		ConnectTimeout: cfg.DB.ConnectTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("admin runner %s: db open: %w", op, err)
	}
	return pool, nil
}

func parseRunnerLabels(raw string) ([]string, error) {
	return runnerlabels.ParseCSV(raw)
}

func init() {
	adminCmd.AddCommand(newAdminRunnerCmd())
	adminActionsCmd.AddCommand(newAdminRunnerCmd())
}
