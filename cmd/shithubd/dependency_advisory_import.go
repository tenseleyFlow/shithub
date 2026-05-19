// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/db"
	"github.com/tenseleyFlow/shithub/internal/repos/advisoryimport"
	"github.com/tenseleyFlow/shithub/internal/worker"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

var dependencyAdvisoryImportOSVFlags struct {
	filePath    string
	sourceName  string
	sourceURL   string
	license     string
	attribution string
	maxBytes    int64
}

// dependencyAdvisoryImportOSVCmd enqueues an operator-controlled OSV import.
// The worker reads a local file only; shithub request handlers never call out
// to external advisory APIs.
var dependencyAdvisoryImportOSVCmd = &cobra.Command{
	Use:   "dependency-advisories-import-osv --file path/to/osv.json",
	Short: "Enqueue an OSV advisory import from a local file",
	Long: `Enqueues dependency_advisory:osv_import for an operator-provided OSV
JSON file. The importer accepts either one OSV object or an array of OSV
objects, stores source/alias/affected-range intelligence in the local advisory
catalog, and records sync-run telemetry.

This command never fetches remote data. Download, review, and license-check
the advisory source out of band, then pass the approved local file here.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		filePath := strings.TrimSpace(dependencyAdvisoryImportOSVFlags.filePath)
		if filePath == "" {
			return errors.New("dependency-advisories-import-osv: --file is required")
		}
		abs, err := filepath.Abs(filePath)
		if err != nil {
			return fmt.Errorf("dependency-advisories-import-osv: resolve --file: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("dependency-advisories-import-osv: stat --file: %w", err)
		}
		if info.IsDir() {
			return errors.New("dependency-advisories-import-osv: --file cannot be a directory")
		}

		sourceName := strings.TrimSpace(dependencyAdvisoryImportOSVFlags.sourceName)
		if sourceName == "" {
			return errors.New("dependency-advisories-import-osv: --source-name is required")
		}
		maxBytes := dependencyAdvisoryImportOSVFlags.maxBytes
		if maxBytes < 0 {
			return errors.New("dependency-advisories-import-osv: --max-bytes must be non-negative")
		}
		if maxBytes == 0 {
			maxBytes = advisoryimport.DefaultMaxOSVImportBytes
		}

		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		if cfg.DB.URL == "" {
			return errors.New("dependency-advisories-import-osv: DB not configured")
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

		payload := jobs.DependencyAdvisoryOSVImportPayload{
			FilePath:    abs,
			SourceName:  sourceName,
			SourceURL:   strings.TrimSpace(dependencyAdvisoryImportOSVFlags.sourceURL),
			License:     strings.TrimSpace(dependencyAdvisoryImportOSVFlags.license),
			Attribution: strings.TrimSpace(dependencyAdvisoryImportOSVFlags.attribution),
			MaxBytes:    maxBytes,
		}
		jobID, err := worker.Enqueue(ctx, pool, worker.KindDependencyAdvisoryOSVImport, payload, worker.EnqueueOptions{})
		if err != nil {
			return err
		}
		if err := worker.Notify(ctx, pool); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: worker notify failed: %v\n", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"dependency-advisories-import-osv: enqueued job %d for %s; run repo-dependencies-backfill-all after import if existing inventories need alert recomputation\n",
			jobID, abs)
		return nil
	},
}

func init() {
	dependencyAdvisoryImportOSVCmd.Flags().StringVar(&dependencyAdvisoryImportOSVFlags.filePath, "file", "", "Local OSV JSON file to import")
	dependencyAdvisoryImportOSVCmd.Flags().StringVar(&dependencyAdvisoryImportOSVFlags.sourceName, "source-name", "osv", "Local advisory source name")
	dependencyAdvisoryImportOSVCmd.Flags().StringVar(&dependencyAdvisoryImportOSVFlags.sourceURL, "source-url", "https://osv.dev", "Source URL for imported advisories")
	dependencyAdvisoryImportOSVCmd.Flags().StringVar(&dependencyAdvisoryImportOSVFlags.license, "license", "", "Source license label")
	dependencyAdvisoryImportOSVCmd.Flags().StringVar(&dependencyAdvisoryImportOSVFlags.attribution, "attribution", "", "Source attribution text")
	dependencyAdvisoryImportOSVCmd.Flags().Int64Var(&dependencyAdvisoryImportOSVFlags.maxBytes, "max-bytes", advisoryimport.DefaultMaxOSVImportBytes, "Maximum OSV import file size in bytes")
}
