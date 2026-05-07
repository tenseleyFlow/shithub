// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
)

var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Load development seed SQL files from seeds/dev/ in lex order",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		return runSeed(cmd.Context(), dir)
	},
}

func init() {
	seedCmd.Flags().StringP("dir", "d", "seeds/dev", "Directory containing .sql seed files")
}

func runSeed(ctx context.Context, dir string) error {
	pool, err := db.Open(ctx, db.Defaults())
	if err != nil {
		return err
	}
	defer pool.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("seed: read %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "seed: no .sql files in", dir, "(nothing to do)")
		return nil
	}

	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // operator-supplied path
		if err != nil {
			return fmt.Errorf("seed: read %s: %w", f, err)
		}
		fmt.Fprintln(os.Stderr, "seed: applying", f)
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("seed: %s: %w", f, err)
		}
	}
	fmt.Fprintln(os.Stderr, "seed: done")
	return nil
}
