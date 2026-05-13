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

	"github.com/jackc/pgx/v5"
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
		Short: "Register and operate Actions runners",
	}
	cmd.AddCommand(newAdminRunnerRegisterCmd())
	cmd.AddCommand(newAdminRunnerListCmd())
	cmd.AddCommand(newAdminRunnerQueueCmd())
	cmd.AddCommand(newAdminRunnerDrainCmd())
	cmd.AddCommand(newAdminRunnerUndrainCmd())
	cmd.AddCommand(newAdminRunnerRotateTokenCmd())
	cmd.AddCommand(newAdminRunnerRevokeCmd())
	cmd.AddCommand(newAdminRunnerCleanupStaleCmd())
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
	return writeRunnerTokenOutput(w, format, "runner registered", out)
}

func writeRunnerTokenOutput(w io.Writer, format, heading string, out runnerRegisterOutput) error {
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
		"%s\nid: %d\nname: %s\nlabels: %s\ncapacity: %d\ntoken_expires_at: %s\ntoken: %s\n\nStore this token now; shithub never shows it again.\n",
		heading, out.ID, out.Name, strings.Join(out.Labels, ","), out.Capacity, expires, out.Token)
	return err
}

func newAdminRunnerListCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered Actions runners",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			output, err = normalizeRunnerOutput("admin runner list", output)
			if err != nil {
				return err
			}
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
			return writeRunnerListOutput(cmd.OutOrStdout(), output, rows, time.Now().UTC())
		},
	}
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

type runnerListOutputRow struct {
	ID                      int64    `json:"id"`
	Name                    string   `json:"name"`
	Status                  string   `json:"status"`
	Capacity                int32    `json:"capacity"`
	ActiveJobCount          int32    `json:"active_job_count"`
	Labels                  []string `json:"labels"`
	HostName                string   `json:"host_name,omitempty"`
	Version                 string   `json:"version,omitempty"`
	LastHeartbeatAt         string   `json:"last_heartbeat_at,omitempty"`
	LastHeartbeatAgeSeconds int64    `json:"last_heartbeat_age_seconds,omitempty"`
	DrainingAt              string   `json:"draining_at,omitempty"`
	DrainReason             string   `json:"drain_reason,omitempty"`
	RevokedAt               string   `json:"revoked_at,omitempty"`
	RevokedReason           string   `json:"revoked_reason,omitempty"`
	CreatedAt               string   `json:"created_at,omitempty"`
}

func writeRunnerListOutput(w io.Writer, format string, rows []actionsdb.ListRunnersRow, now time.Time) error {
	out := make([]runnerListOutputRow, 0, len(rows))
	for _, row := range rows {
		item := runnerListOutputRow{
			ID:             row.ID,
			Name:           row.Name,
			Status:         string(row.Status),
			Capacity:       row.Capacity,
			ActiveJobCount: row.ActiveJobCount,
			Labels:         append([]string{}, row.Labels...),
			HostName:       row.HostName,
			Version:        row.Version,
		}
		if row.LastHeartbeatAt.Valid {
			item.LastHeartbeatAt = row.LastHeartbeatAt.Time.UTC().Format(time.RFC3339)
			if d := now.Sub(row.LastHeartbeatAt.Time); d > 0 {
				item.LastHeartbeatAgeSeconds = int64(d.Seconds())
			}
		}
		if row.DrainingAt.Valid {
			item.DrainingAt = row.DrainingAt.Time.UTC().Format(time.RFC3339)
			item.DrainReason = row.DrainReason
		}
		if row.RevokedAt.Valid {
			item.RevokedAt = row.RevokedAt.Time.UTC().Format(time.RFC3339)
			item.RevokedReason = row.RevokedReason
		}
		if row.CreatedAt.Valid {
			item.CreatedAt = row.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tCAPACITY\tACTIVE\tLABELS\tHOST\tVERSION\tLAST_HEARTBEAT\tDRAINING\tREVOKED")
	for _, row := range out {
		last := "never"
		if row.LastHeartbeatAt != "" {
			last = row.LastHeartbeatAt
		}
		draining := "-"
		if row.DrainingAt != "" {
			draining = row.DrainingAt
		}
		revoked := "-"
		if row.RevokedAt != "" {
			revoked = row.RevokedAt
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.ID, row.Name, row.Status, row.Capacity, row.ActiveJobCount, strings.Join(row.Labels, ","),
			emptyDash(row.HostName), emptyDash(row.Version), last, draining, revoked)
	}
	return tw.Flush()
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

func newAdminRunnerDrainCmd() *cobra.Command {
	var idRaw string
	var reason string
	var output string
	cmd := &cobra.Command{
		Use:   "drain --id <id> [--reason <text>]",
		Short: "Stop an Actions runner from claiming new jobs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := parseRunnerID("admin runner drain", idRaw)
			if err != nil {
				return err
			}
			output, err = normalizeRunnerOutput("admin runner drain", output)
			if err != nil {
				return err
			}
			reason, err = normalizeRunnerReason(reason, "operator requested drain")
			if err != nil {
				return err
			}
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "drain")
			if err != nil {
				return err
			}
			defer pool.Close()

			row, err := actionsdb.New().SetRunnerDraining(ctx, pool, actionsdb.SetRunnerDrainingParams{
				ID:          id,
				DrainReason: reason,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("admin runner drain: runner %d not found or already revoked", id)
				}
				return fmt.Errorf("admin runner drain: %w", err)
			}
			return writeRunnerStateOutput(cmd.OutOrStdout(), output, "runner draining", runnerStateOutput{
				ID:          row.ID,
				Name:        row.Name,
				Status:      string(row.Status),
				DrainingAt:  formatOptionalTime(row.DrainingAt),
				DrainReason: row.DrainReason,
				RevokedAt:   formatOptionalTime(row.RevokedAt),
			})
		},
	}
	cmd.Flags().StringVar(&idRaw, "id", "", "Runner id")
	cmd.Flags().StringVar(&reason, "reason", "", "Drain reason recorded for operators")
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

func newAdminRunnerUndrainCmd() *cobra.Command {
	var idRaw string
	var output string
	cmd := &cobra.Command{
		Use:   "undrain --id <id>",
		Short: "Allow a drained Actions runner to claim jobs again",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := parseRunnerID("admin runner undrain", idRaw)
			if err != nil {
				return err
			}
			output, err = normalizeRunnerOutput("admin runner undrain", output)
			if err != nil {
				return err
			}
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "undrain")
			if err != nil {
				return err
			}
			defer pool.Close()

			row, err := actionsdb.New().ClearRunnerDraining(ctx, pool, id)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("admin runner undrain: runner %d not found or already revoked", id)
				}
				return fmt.Errorf("admin runner undrain: %w", err)
			}
			return writeRunnerStateOutput(cmd.OutOrStdout(), output, "runner undrained", runnerStateOutput{
				ID:          row.ID,
				Name:        row.Name,
				Status:      string(row.Status),
				DrainingAt:  formatOptionalTime(row.DrainingAt),
				DrainReason: row.DrainReason,
				RevokedAt:   formatOptionalTime(row.RevokedAt),
			})
		},
	}
	cmd.Flags().StringVar(&idRaw, "id", "", "Runner id")
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

func newAdminRunnerRotateTokenCmd() *cobra.Command {
	var idRaw string
	var output string
	var expiresIn time.Duration
	cmd := &cobra.Command{
		Use:   "rotate-token --id <id>",
		Short: "Revoke existing registration tokens and print one replacement token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := parseRunnerID("admin runner rotate-token", idRaw)
			if err != nil {
				return err
			}
			output, err = normalizeRunnerOutput("admin runner rotate-token", output)
			if err != nil {
				return err
			}
			if expiresIn < 0 {
				return errors.New("admin runner rotate-token: --expires-in must be non-negative")
			}
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "rotate-token")
			if err != nil {
				return err
			}
			defer pool.Close()

			token, tokenHash, err := runnertoken.New()
			if err != nil {
				return fmt.Errorf("admin runner rotate-token: mint token: %w", err)
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
				return fmt.Errorf("admin runner rotate-token: begin: %w", err)
			}
			committed := false
			defer func() {
				if !committed {
					_ = tx.Rollback(ctx)
				}
			}()
			runner, err := q.LockRunnerByID(ctx, tx, id)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("admin runner rotate-token: runner %d not found", id)
				}
				return fmt.Errorf("admin runner rotate-token: lock runner: %w", err)
			}
			if runner.RevokedAt.Valid {
				return fmt.Errorf("admin runner rotate-token: runner %d is revoked", id)
			}
			if err := q.RevokeAllTokensForRunner(ctx, tx, id); err != nil {
				return fmt.Errorf("admin runner rotate-token: revoke old tokens: %w", err)
			}
			if _, err := q.InsertRunnerToken(ctx, tx, actionsdb.InsertRunnerTokenParams{
				RunnerID:  runner.ID,
				TokenHash: tokenHash,
				ExpiresAt: expiresAt,
			}); err != nil {
				return fmt.Errorf("admin runner rotate-token: insert token: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("admin runner rotate-token: commit: %w", err)
			}
			committed = true

			return writeRunnerTokenOutput(cmd.OutOrStdout(), output, "runner token rotated", runnerRegisterOutput{
				ID:             runner.ID,
				Name:           runner.Name,
				Labels:         runner.Labels,
				Capacity:       runner.Capacity,
				Token:          token,
				TokenExpiresAt: outputExpiresAt,
			})
		},
	}
	cmd.Flags().StringVar(&idRaw, "id", "", "Runner id")
	cmd.Flags().DurationVar(&expiresIn, "expires-in", 0, "Registration token lifetime (0 means no expiration)")
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

func newAdminRunnerRevokeCmd() *cobra.Command {
	var idRaw string
	var reason string
	var output string
	cmd := &cobra.Command{
		Use:   "revoke --id <id>",
		Short: "Hard-revoke an Actions runner and all registration tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := parseRunnerID("admin runner revoke", idRaw)
			if err != nil {
				return err
			}
			output, err = normalizeRunnerOutput("admin runner revoke", output)
			if err != nil {
				return err
			}
			reason, err = normalizeRunnerReason(reason, "operator requested revoke")
			if err != nil {
				return err
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
			tx, err := pool.Begin(ctx)
			if err != nil {
				return fmt.Errorf("admin runner revoke: begin: %w", err)
			}
			committed := false
			defer func() {
				if !committed {
					_ = tx.Rollback(ctx)
				}
			}()
			runner, err := q.RevokeRunner(ctx, tx, actionsdb.RevokeRunnerParams{
				ID:            id,
				RevokedReason: reason,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("admin runner revoke: runner %d not found", id)
				}
				return fmt.Errorf("admin runner revoke: %w", err)
			}
			if err := q.RevokeAllTokensForRunner(ctx, tx, id); err != nil {
				return fmt.Errorf("admin runner revoke: revoke tokens: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("admin runner revoke: commit: %w", err)
			}
			committed = true
			metrics.ActionsRunnerRevocationsTotal.Inc()
			return writeRunnerStateOutput(cmd.OutOrStdout(), output, "runner revoked", runnerStateOutput{
				ID:            runner.ID,
				Name:          runner.Name,
				Status:        string(runner.Status),
				DrainingAt:    formatOptionalTime(runner.DrainingAt),
				DrainReason:   runner.DrainReason,
				RevokedAt:     formatOptionalTime(runner.RevokedAt),
				RevokedReason: runner.RevokedReason,
			})
		},
	}
	cmd.Flags().StringVar(&idRaw, "id", "", "Runner id")
	cmd.Flags().StringVar(&reason, "reason", "", "Revocation reason recorded for operators")
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

func newAdminRunnerCleanupStaleCmd() *cobra.Command {
	var olderThan time.Duration
	var output string
	cmd := &cobra.Command{
		Use:   "cleanup-stale",
		Short: "Mark stale non-revoked runners offline",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			output, err = normalizeRunnerOutput("admin runner cleanup-stale", output)
			if err != nil {
				return err
			}
			if olderThan <= 0 {
				return errors.New("admin runner cleanup-stale: --older-than must be positive")
			}
			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			pool, err := openAdminRunnerPool(ctx, cfg, "cleanup-stale")
			if err != nil {
				return err
			}
			defer pool.Close()

			cutoff := time.Now().UTC().Add(-olderThan)
			rows, err := actionsdb.New().MarkStaleRunnersOffline(ctx, pool, pgtype.Timestamptz{Time: cutoff, Valid: true})
			if err != nil {
				return fmt.Errorf("admin runner cleanup-stale: %w", err)
			}
			return writeRunnerCleanupOutput(cmd.OutOrStdout(), output, rows)
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 2*time.Minute, "Heartbeat age after which non-revoked runners are marked offline")
	cmd.Flags().StringVar(&output, "output", "text", "Output format: text or json")
	return cmd
}

type runnerStateOutput struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	DrainingAt    string `json:"draining_at,omitempty"`
	DrainReason   string `json:"drain_reason,omitempty"`
	RevokedAt     string `json:"revoked_at,omitempty"`
	RevokedReason string `json:"revoked_reason,omitempty"`
}

func writeRunnerStateOutput(w io.Writer, format, heading string, out runnerStateOutput) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	_, err := fmt.Fprintf(w, "%s\nid: %d\nname: %s\nstatus: %s\ndraining_at: %s\ndrain_reason: %s\nrevoked_at: %s\nrevoked_reason: %s\n",
		heading, out.ID, out.Name, out.Status, emptyDash(out.DrainingAt), emptyDash(out.DrainReason),
		emptyDash(out.RevokedAt), emptyDash(out.RevokedReason))
	return err
}

type runnerCleanupOutputRow struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	LastHeartbeatAt string `json:"last_heartbeat_at,omitempty"`
}

func writeRunnerCleanupOutput(w io.Writer, format string, rows []actionsdb.MarkStaleRunnersOfflineRow) error {
	out := make([]runnerCleanupOutputRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, runnerCleanupOutputRow{
			ID:              row.ID,
			Name:            row.Name,
			Status:          string(row.Status),
			LastHeartbeatAt: formatOptionalTime(row.LastHeartbeatAt),
		})
	}
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tLAST_HEARTBEAT")
	for _, row := range out {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", row.ID, row.Name, row.Status, emptyDash(row.LastHeartbeatAt))
	}
	return tw.Flush()
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

func parseRunnerID(op, raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%s: --id must be a positive integer", op)
	}
	return id, nil
}

func normalizeRunnerOutput(op, output string) (string, error) {
	output = strings.ToLower(strings.TrimSpace(output))
	switch output {
	case "", "text":
		return "text", nil
	case "json":
		return "json", nil
	default:
		return "", fmt.Errorf("%s: --output must be text or json", op)
	}
}

func normalizeRunnerReason(reason, fallback string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = fallback
	}
	if len(reason) > 1000 {
		return "", errors.New("runner reason must be 1000 bytes or fewer")
	}
	return reason, nil
}

func formatOptionalTime(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func init() {
	adminCmd.AddCommand(newAdminRunnerCmd())
	adminActionsCmd.AddCommand(newAdminRunnerCmd())
}
