// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
)

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Storage health checks and operational helpers",
}

var storageCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Verify object-store round-trip and repos-root writability",
	Long: `Exits 0 when both:
  (a) PUT and GET succeed against the configured S3-compatible object bucket, and
  (b) the configured repos_root is writable.

Production uses DigitalOcean Spaces through that S3-compatible API. When the
S3 block is unconfigured, only (b) is checked. Used in deploy smoke tests and
as a sanity check from the operator's terminal.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load(nil)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		var failures []string

		out := cmd.OutOrStdout()
		if err := checkReposRoot(cfg.Storage.ReposRoot); err != nil {
			failures = append(failures, fmt.Sprintf("repos_root: %v", err))
		} else {
			_, _ = fmt.Fprintf(out, "repos_root: ok (%s)\n", cfg.Storage.ReposRoot)
		}

		if cfg.Storage.S3.Endpoint == "" {
			_, _ = fmt.Fprintln(out, "s3: skipped (storage.s3 not configured)")
		} else if err := checkS3(ctx, cfg.Storage.S3); err != nil {
			failures = append(failures, fmt.Sprintf("s3: %v", err))
		} else {
			_, _ = fmt.Fprintf(out, "s3: ok (endpoint=%s bucket=%s)\n",
				cfg.Storage.S3.Endpoint, cfg.Storage.S3.Bucket)
		}

		if len(failures) > 0 {
			return fmt.Errorf("storage check failed:\n  - %s", strings.Join(failures, "\n  - "))
		}
		return nil
	},
}

func init() {
	storageCmd.AddCommand(storageCheckCmd)
}

func checkReposRoot(root string) error {
	if root == "" {
		return errors.New("not configured")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	probe, err := os.CreateTemp(root, ".storage-check-")
	if err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("cleanup probe: %w", err)
	}
	return nil
}

func checkS3(ctx context.Context, s config.S3StorageConfig) error {
	store, err := storage.NewS3Store(storage.S3Config{
		Endpoint:        s.Endpoint,
		Region:          s.Region,
		AccessKeyID:     s.AccessKeyID,
		SecretAccessKey: s.SecretAccessKey,
		Bucket:          s.Bucket,
		UseSSL:          s.UseSSL,
		ForcePathStyle:  s.ForcePathStyle,
	})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("random: %w", err)
	}
	key := "_storage-check/" + hex.EncodeToString(suffix)
	body := []byte("storage check at " + time.Now().UTC().Format(time.RFC3339Nano))

	if _, err := store.Put(ctx, key, strings.NewReader(string(body)), storage.PutOpts{ContentType: "text/plain"}); err != nil {
		return fmt.Errorf("put: %w", err)
	}
	defer func() { _ = store.Delete(ctx, key) }()

	rc, _, err := store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if string(got) != string(body) {
		return errors.New("round-trip body mismatch")
	}
	return nil
}
