// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	cmd.AddCommand(newAdminRunnerQueueCmd())
	cmd.AddCommand(newAdminRunnerRevokeCmd())
	return cmd
}

func newAdminRunnerRegisterCmd() *cobra.Command {
	var name string
	var labelsRaw string
	var capacity int
	var output string
	var expiresIn time.Duration
	cmd := &cobra.Command{
		Use:   "register --name <name> [--labels self-hosted,linux,ubuntu-latest,x64] [--capacity 1]",
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
			output = strings.ToLower(strings.TrimSpace(output))
			switch output {
			case "", "text":
				output = "text"
			case "json":
			default:
				return errors.New("admin runner register: --output must be text or json")
			}
			if expiresIn < 0 {
				return errors.New("admin runner register: --expires-in must be non-negative")
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
			var expiresAt pgtype.Timestamptz
			var outputExpiresAt *time.Time
			if expiresIn > 0 {
				t := time.Now().UTC().Add(expiresIn)
				expiresAt = pgtype.Timestamptz{Time: t, Valid: true}
				outputExpiresAt = &t
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
				ExpiresAt: expiresAt,
			}); err != nil {
				return fmt.Errorf("admin runner register: insert token: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("admin runner register: commit: %w", err)
			}
			committed = true
			metrics.ActionsRunnerRegistrationsTotal.Inc()

			return writeRunnerRegisterOutput(cmd.OutOrStdout(), output, runnerRegisterOutput{
				ID:             runner.ID,
				Name:           runner.Name,
				Labels:         runner.Labels,
				Capacity:       runner.Capacity,
				Token:          token,
				TokenExpiresAt: outputExpiresAt,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Runner name (letters, numbers, underscore, dash)")
	cmd.Flags().StringVar(&labelsRaw, "labels", strings.Join(runnerlabels.DefaultShared(), ","), "Comma-separated runner labels")
	cmd.Flags().IntVar(&capacity, "capacity", 1, "Maximum concurrent jobs this runner may execute")
	cmd.Flags().DurationVar(&expiresIn, "expires-in", 0, "Registration token lifetime (0 means no expiration)")
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

type runnerRegisterOutput struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Labels         []string   `json:"labels"`
	Capacity       int32      `json:"capacity"`
	Token          string     `json:"token"`
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
}

func writeRunnerRegisterOutput(w io.Writer, format string, out runnerRegisterOutput) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	expires := "never"
	if out.TokenExpiresAt != nil {
		expires = out.TokenExpiresAt.Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(w,
		"runner registered\nid: %d\nname: %s\nlabels: %s\ncapacity: %d\ntoken_expires_at: %s\ntoken: %s\n\nStore this token now; shithub never shows it again.\n",
		out.ID, out.Name, strings.Join(out.Labels, ","), out.Capacity, expires, out.Token)
	return err
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

func newAdminRunnerQueueCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Summarize queued Actions jobs by runs-on label",
		RunE: func(cmd *cobra.Command, _ []string) error {
			output = strings.ToLower(strings.TrimSpace(output))
			switch output {
			case "", "text":
				output = "text"
			case "json":
			default:
				return errors.New("admin runner queue: --output must be text or json")
			}
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "queue")
			if err != nil {
				return err
			}
			defer pool.Close()

			rows, err := actionsdb.New().ListQueuedWorkflowJobRunsOn(ctx, pool)
			if err != nil {
				return fmt.Errorf("admin runner queue: %w", err)
			}
			return writeRunnerQueueOutput(cmd.OutOrStdout(), output, rows, time.Now().UTC())
		},
	}
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

type runnerQueueOutputRow struct {
	RunsOn              string `json:"runs_on"`
	QueuedJobs          int32  `json:"queued_jobs"`
	MatchingRunnerCount int32  `json:"matching_runner_count"`
	OldestQueuedAt      string `json:"oldest_queued_at,omitempty"`
	OldestQueuedSeconds int64  `json:"oldest_queued_seconds,omitempty"`
}

func writeRunnerQueueOutput(w io.Writer, format string, rows []actionsdb.ListQueuedWorkflowJobRunsOnRow, now time.Time) error {
	out := make([]runnerQueueOutputRow, 0, len(rows))
	for _, row := range rows {
		item := runnerQueueOutputRow{
			RunsOn:              row.RunsOn,
			QueuedJobs:          row.QueuedJobs,
			MatchingRunnerCount: row.MatchingRunnerCount,
		}
		if row.OldestQueuedAt.Valid {
			item.OldestQueuedAt = row.OldestQueuedAt.Time.UTC().Format(time.RFC3339)
			if d := now.Sub(row.OldestQueuedAt.Time); d > 0 {
				item.OldestQueuedSeconds = int64(d.Seconds())
			}
		}
		out = append(out, item)
	}
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RUNS_ON\tQUEUED_JOBS\tMATCHING_RUNNERS\tOLDEST_QUEUED")
	for _, row := range out {
		oldest := "-"
		if row.OldestQueuedAt != "" {
			oldest = row.OldestQueuedAt
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", row.RunsOn, row.QueuedJobs, row.MatchingRunnerCount, oldest)
	}
	return tw.Flush()
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
