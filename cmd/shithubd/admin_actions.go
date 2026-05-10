// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/actions/workflow"
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

func init() {
	adminActionsCmd.AddCommand(adminActionsParseCmd)
	adminCmd.AddCommand(adminActionsCmd)
}
