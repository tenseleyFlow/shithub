// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
)

// adminActionsCmd is the parent group for actions-related operator
// subcommands. Currently scoped to read-only inspection (`parse`); will
// grow with `runner register`, `runner list`, etc. in S41c.
var adminActionsCmd = &cobra.Command{
	Use:   "actions",
	Short: "Inspect and operate the Actions/CI subsystem",
}

// adminActionsParseCmd reads a workflow YAML file from disk, runs it
// through the parser, and prints diagnostics + a canonical JSON
// rendering of the parsed AST. No DB or runner dependency — this is
// the smoke command operators run when a workflow misbehaves and we
// want a deterministic dump of what the parser actually saw.
//
// Exit codes:
//   - 0: parse succeeded and produced no Error-severity diagnostics
//   - 1: parse error (file too large, malformed YAML, IO error)
//   - 2: parse produced one or more Error-severity diagnostics
var adminActionsParseCmd = &cobra.Command{
	Use:   "parse <file>",
	Short: "Parse a workflow YAML file and print diagnostics + canonical JSON",
	Long: `Reads a workflow file, runs the v1 parser against it, and prints any
diagnostics followed by a canonical JSON rendering of the parsed AST.

Useful for:
  - debugging "why is my workflow not picking up changes" reports
  - validating a workflow file before committing it
  - producing a stable AST snapshot for inclusion in bug reports

Exit code 0 = clean parse, 2 = Error-severity diagnostics produced,
1 = the file itself was unreadable or oversized.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		wf, diags, err := workflow.Parse(src)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		out := cmd.OutOrStdout()
		errs := 0
		for _, d := range diags {
			fmt.Fprintln(out, d.String())
			if d.Severity == workflow.Error {
				errs++
			}
		}
		if len(diags) > 0 {
			fmt.Fprintln(out, "---")
		}

		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(wf); err != nil {
			return fmt.Errorf("encode AST: %w", err)
		}

		if errs > 0 {
			os.Exit(2)
		}
		return nil
	},
}

func newAdminActionsCancelAllCmd() *cobra.Command {
	var repoID int64
	var limit int
	var dryRun bool
	var confirm bool

	cmd := &cobra.Command{
		Use:   "cancel-all",
		Short: "Request cancellation for active Actions workflow runs",
		Long: `Requests cancellation for queued/running Actions workflow runs.

By default this scans all repositories, oldest first. Use --repo-id to scope
the operation to one repository. Running jobs receive cancel_requested=true and
are killed by their runner's cancel-check loop; queued jobs become terminal
immediately.

This is an operator break-glass command. Run with --dry-run first, then repeat
with --confirm to mutate state.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repoID < 0 {
				return errors.New("admin actions cancel-all: --repo-id must be zero or positive")
			}
			if limit < 1 || limit > 5000 {
				return errors.New("admin actions cancel-all: --limit must be between 1 and 5000")
			}
			if !dryRun && !confirm {
				return errors.New("admin actions cancel-all: refusing to mutate without --confirm; use --dry-run to inspect")
			}

			cfg, err := config.Load(nil)
			if err != nil {
				return err
			}
			if cfg.DB.URL == "" {
				return errors.New("admin actions cancel-all: DB not configured (set SHITHUB_DATABASE_URL)")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			pool, err := db.Open(ctx, db.Config{
				URL:            cfg.DB.URL,
				MaxConns:       2,
				MinConns:       0,
				ConnectTimeout: cfg.DB.ConnectTimeout,
			})
			if err != nil {
				return fmt.Errorf("admin actions cancel-all: db open: %w", err)
			}
			defer pool.Close()

			q := actionsdb.New()
			runs, err := q.ListActiveWorkflowRunsForAdmin(ctx, pool, actionsdb.ListActiveWorkflowRunsForAdminParams{
				RepoID:     repoID,
				LimitCount: int32(limit),
			})
			if err != nil {
				return fmt.Errorf("admin actions cancel-all: list active runs: %w", err)
			}

			out := cmd.OutOrStdout()
			scope := "all repositories"
			if repoID != 0 {
				scope = fmt.Sprintf("repo_id=%d", repoID)
			}
			if dryRun {
				for _, run := range runs {
					_, _ = fmt.Fprintf(out,
						"would cancel: run_id=%d repo_id=%d run_index=%d status=%s workflow=%s ref=%s sha=%s\n",
						run.ID, run.RepoID, run.RunIndex, run.Status, run.WorkflowFile, run.HeadRef, run.HeadSha)
				}
				_, _ = fmt.Fprintf(out, "cancel-all dry-run: found %d active run(s) in %s (limit=%d)\n",
					len(runs), scope, limit)
				return nil
			}

			cancelledRuns := 0
			cancelledJobs := 0
			completedRuns := 0
			for _, run := range runs {
				result, err := actionslifecycle.CancelRun(ctx, actionslifecycle.Deps{Pool: pool}, run.ID, actionslifecycle.CancelReasonUser)
				if err != nil {
					return fmt.Errorf("admin actions cancel-all: cancel run %d: %w", run.ID, err)
				}
				if len(result.ChangedJobs) > 0 {
					cancelledRuns++
				}
				cancelledJobs += len(result.ChangedJobs)
				if result.RunCompleted {
					completedRuns++
				}
				_, _ = fmt.Fprintf(out,
					"cancelled: run_id=%d repo_id=%d run_index=%d changed_jobs=%d run_completed=%t\n",
					run.ID, run.RepoID, run.RunIndex, len(result.ChangedJobs), result.RunCompleted)
			}
			_, _ = fmt.Fprintf(out,
				"cancel-all: scanned %d active run(s) in %s; changed_runs=%d changed_jobs=%d completed_runs=%d\n",
				len(runs), scope, cancelledRuns, cancelledJobs, completedRuns)
			return nil
		},
	}
	cmd.Flags().Int64Var(&repoID, "repo-id", 0, "Repository id to scope cancellation to; 0 scans all repositories")
	cmd.Flags().IntVar(&limit, "limit", 500, "Maximum active runs to scan")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print matching active runs without mutating state")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm cancellation of matching active runs")
	return cmd
}

func init() {
	adminActionsCmd.AddCommand(adminActionsParseCmd)
	adminActionsCmd.AddCommand(newAdminActionsCancelAllCmd())
	adminCmd.AddCommand(adminActionsCmd)
}
