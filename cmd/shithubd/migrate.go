// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
	_ "github.com/tenseleyFlow/shithub/internal/migrationsfs" // registers the embedded migrations FS with db
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations",
	Long:  "Run database migrations against SHITHUB_DATABASE_URL using goose. Subcommands: up, down, status, version, redo, reset.",
}

func newMigrateActionCmd(use, short string, action db.MigrateAction) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return db.Migrate(cmd.Context(), db.Defaults(), action)
		},
	}
}

func init() {
	migrateCmd.AddCommand(newMigrateActionCmd("up", "Apply all pending migrations", db.MigrateUp))
	migrateCmd.AddCommand(newMigrateActionCmd("down", "Roll back the most recent migration", db.MigrateDown))
	migrateCmd.AddCommand(newMigrateActionCmd("status", "Show applied/pending migrations", db.MigrateStatus))
	migrateCmd.AddCommand(newMigrateActionCmd("version", "Show current schema version", db.MigrateVersion))
	migrateCmd.AddCommand(newMigrateActionCmd("redo", "Re-apply the most recent migration", db.MigrateRedo))
	migrateCmd.AddCommand(newMigrateActionCmd("reset", "Roll back all migrations (DANGEROUS)", db.MigrateReset))
}
